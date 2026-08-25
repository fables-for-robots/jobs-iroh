package serve

import (
	"context"
	"encoding/hex"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"path/filepath"
	"strings"

	"github.com/fxamacker/cbor/v2"
	"github.com/tmc/go-iroh/iroh"
	"golang.org/x/sync/errgroup"

	"github.com/jobs-build/jobs-iroh/amber"
	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/sched"
)

// apiService serves the api frame protocol (4-byte BE length + CBOR
// {t, b}) of one client ALPN over the scheduler. The build ALPN accepts
// submit/watch/logs/cancel; the admin ALPN additionally accepts
// requests/fleet/stats/refs/delete/diagnose (admin=true).
type apiService struct {
	log      *slog.Logger
	sd       *sched.Sched
	store    *amber.Store
	storeDir string // for the stats disk-usage walk
	gc       *gcRunner
	admin    bool
}

// handler is the per-connection entry: a stream-accept loop with one
// goroutine per stream — the api contract is one request per stream, with
// watch/log streams staying open until terminal or client close.
func (svc *apiService) handler() iroh.ProtocolHandler {
	return iroh.ProtocolHandlerFunc(func(ctx context.Context, conn *iroh.Conn) error {
		g, ctx := errgroup.WithContext(ctx)
		// Closing the connection on ctx cancel unblocks in-flight streams.
		stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stop()
		remote := conn.RemoteID().String()
		for {
			stream, err := conn.AcceptStreamConn(ctx)
			if err != nil {
				break // peer closed the connection, or ctx cancelled
			}
			g.Go(func() error {
				svc.serveStream(ctx, remote, stream)
				return nil
			})
		}
		return g.Wait()
	})
}

// serveStream reads the stream's single request frame, dispatches it, and
// maps any error onto a terminal api.Error frame.
func (svc *apiService) serveStream(ctx context.Context, remote string, stream net.Conn) {
	defer stream.Close()
	// Terminate the receive side too (Close only ends the send side): it
	// retires the stream for MAX_STREAMS credit without waiting on the
	// client's FIN and unblocks the one-request watchdog read below.
	defer func() {
		if cr, ok := stream.(interface{ CancelRead(code uint64) }); ok {
			cr.CancelRead(0)
		}
	}()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()

	t, body, err := api.ReadFrame(stream)
	if err != nil {
		return // client vanished before asking anything
	}
	// One request per stream: any further read result (EOF, reset, junk)
	// means the client is gone or broken — cancel the server-side work
	// feeding this stream (long watch/log follows in particular).
	go func() {
		var b [1]byte
		_, _ = stream.Read(b[:])
		cancel()
	}()

	if err := svc.dispatch(sctx, remote, t, body, stream); err != nil {
		_ = api.WriteFrame(stream, api.TError, api.Error{
			Code: sched.ErrorCode(err),
			Text: err.Error(),
		})
	}
}

// dispatch routes one request frame. Success paths write their own reply
// frames and return nil; a returned error becomes the stream's api.Error
// frame. Reply-write failures return nil — the stream is already dead and
// the deferred close is all that is left to do.
func (svc *apiService) dispatch(ctx context.Context, remote, t string, body cbor.RawMessage, stream net.Conn) error {
	switch t {
	case api.TSubmit:
		var req api.SubmitRequest
		if err := api.DecodeBody(body, &req); err != nil {
			return badFrame(t, err)
		}
		sub, err := svc.sd.Submit(ctx, req)
		if err != nil {
			return err
		}
		svc.log.Info("client submit",
			"remote", remote, "request", sub.RequestID, "k", hex.EncodeToString(sub.K))
		_ = api.WriteFrame(stream, api.TSubmitted, sub)
		return nil

	case api.TWatch:
		var req api.WatchRequest
		if err := api.DecodeBody(body, &req); err != nil {
			return badFrame(t, err)
		}
		ch, stop, err := svc.sd.Watch(ctx, req.RequestID)
		if err != nil {
			return err
		}
		defer stop()
		for snap := range ch {
			if err := api.WriteFrame(stream, api.TSnapshot, snap); err != nil {
				return nil
			}
		}
		return nil

	case api.TLogs:
		var req api.LogsRequest
		if err := api.DecodeBody(body, &req); err != nil {
			return badFrame(t, err)
		}
		view, ch, stop, err := svc.sd.Logs(ctx, req.Node, req.Follow)
		if err != nil {
			return err
		}
		defer stop()
		if err := api.WriteFrame(stream, api.TLogView, view); err != nil {
			return nil
		}
		for chunk := range ch {
			if err := api.WriteFrame(stream, api.TLogChunk, chunk); err != nil {
				return nil
			}
		}
		return nil

	case api.TCancel:
		var req api.CancelRequest
		if err := api.DecodeBody(body, &req); err != nil {
			return badFrame(t, err)
		}
		if err := svc.sd.Cancel(ctx, req.RequestID); err != nil {
			return err
		}
		svc.log.Info("client cancel", "remote", remote, "request", req.RequestID)
		_ = api.WriteFrame(stream, api.TOK, nil)
		return nil
	}

	if !svc.admin {
		return badRequest("frame type %q is not served on the build ALPN", t)
	}

	switch t {
	case api.TRequests:
		_ = api.WriteFrame(stream, api.TRequestsReply, api.RequestsReply{Requests: svc.sd.Requests()})
		return nil

	case api.TFleet:
		_ = api.WriteFrame(stream, api.TFleetReply, api.FleetReply{Runners: svc.sd.Fleet()})
		return nil

	case api.TStats:
		st := svc.sd.Stats()
		st.StoreBytes = dirSize(svc.storeDir)
		_ = api.WriteFrame(stream, api.TStatsReply, st)
		return nil

	case api.TRefs:
		var req api.RefsRequest
		if len(body) > 0 {
			if err := api.DecodeBody(body, &req); err != nil {
				return badFrame(t, err)
			}
		}
		refs, err := svc.store.ListRefs(ctx)
		if err != nil {
			return err
		}
		out := make([]api.RefInfo, 0, len(refs))
		for _, r := range refs {
			if req.Prefix != "" && !strings.HasPrefix(r.Name, req.Prefix) {
				continue
			}
			k := r.Key
			out = append(out, api.RefInfo{
				Name:      r.Name,
				Key:       k[:],
				CreatedNs: r.CreatedAt.UnixNano(),
			})
			if req.Limit > 0 && len(out) == req.Limit {
				break
			}
		}
		_ = api.WriteFrame(stream, api.TRefsReply, api.RefsReply{Refs: out})
		return nil

	case api.TDelete:
		var req api.CancelRequest
		if err := api.DecodeBody(body, &req); err != nil {
			return badFrame(t, err)
		}
		if err := svc.sd.Delete(ctx, req.RequestID); err != nil {
			return err
		}
		svc.log.Info("client delete", "remote", remote, "request", req.RequestID)
		_ = api.WriteFrame(stream, api.TOK, nil)
		return nil

	case api.TDiagnose:
		var req api.DiagnoseRequest
		if err := api.DecodeBody(body, &req); err != nil {
			return badFrame(t, err)
		}
		reply, err := svc.sd.Diagnose(ctx, req)
		if err != nil {
			return err
		}
		_ = api.WriteFrame(stream, api.TDiagnoseReply, reply)
		return nil

	default:
		return badRequest("unknown frame type %q", t)
	}
}

// badFrame classifies an undecodable request body as a bad request.
func badFrame(t string, err error) error {
	return badRequest("decode %s request: %v", t, err)
}

func badRequest(format string, args ...any) error {
	return &sched.Error{Code: api.CodeBadRequest, Text: fmt.Sprintf(format, args...)}
}

// dirSize sums the regular-file bytes under dir, best-effort: stats belong
// to observation, so a racing compaction or permission hiccup yields a
// smaller number, never an error.
func dirSize(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil || !d.Type().IsRegular() {
			return nil
		}
		if info, err := d.Info(); err == nil {
			total += info.Size()
		}
		return nil
	})
	return total
}
