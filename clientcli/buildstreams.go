package clientcli

// buildStreams adapts one apiConn (+ request id) onto tui.BuildStreams, so
// remote-build and watch can run the full-screen build view over their
// existing build-ALPN connection (docs/design/2026-08-19-remote-build-tui
// .md §3-4). The activation matrix and the old-server peek live here too.

import (
	"context"
	"fmt"
	"os"

	"github.com/urfave/cli/v2"
	"golang.org/x/term"

	"github.com/jobs-build/jobs-iroh/amberclient"
	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/tui"
	"github.com/jobs-build/jobs-iroh/wire"
)

// buildStreams is the tui.BuildStreams over an apiConn.
type buildStreams struct {
	bc    *apiConn
	reqID string
}

var _ tui.BuildStreams = (*buildStreams)(nil)

func (s *buildStreams) OpenWatch(ctx context.Context) (func() (api.Snapshot, error), func(), error) {
	stream, stop, err := s.bc.openRequest(ctx, api.TWatch, api.WatchRequest{RequestID: s.reqID})
	if err != nil {
		return nil, nil, err
	}
	next := func() (api.Snapshot, error) {
		rt, rb, err := api.ReadFrame(stream)
		if err != nil {
			return api.Snapshot{}, err
		}
		var snap api.Snapshot
		if err := decodeReply(rt, rb, api.TSnapshot, &snap); err != nil {
			return api.Snapshot{}, err
		}
		return snap, nil
	}
	return next, func() { stop(); amberclient.CloseStream(stream) }, nil
}

func (s *buildStreams) OpenLogs(ctx context.Context, node string) (api.LogView, func() (wire.LogChunk, error), func(), error) {
	return s.bc.openLogs(ctx, node, true)
}

func (s *buildStreams) Cancel(ctx context.Context) error {
	return s.bc.call(ctx, api.TCancel, api.CancelRequest{RequestID: s.reqID}, api.TOK, nil)
}

// useBuildTUI reports whether the full-screen build view should run: stdin
// AND stderr are TTYs (keyboard + display) and --no-tui is absent. Tests
// with a captured ErrWriter read as non-TTY, like cliLiveView.
func useBuildTUI(c *cli.Context) bool {
	if c.Bool("no-tui") {
		return false
	}
	if w := c.App.ErrWriter; w != nil {
		if f, ok := w.(*os.File); !ok || f != os.Stderr {
			return false
		}
	}
	return term.IsTerminal(int(os.Stdin.Fd())) && term.IsTerminal(int(os.Stderr.Fd()))
}

// peekSnapshot opens a short-lived watch and returns its first snapshot —
// the TUI-or-fallback decision point (an old server sends no graph edges;
// an already-terminal build skips the TUI entirely).
func peekSnapshot(ctx context.Context, bc *apiConn, requestID string) (api.Snapshot, error) {
	stream, stop, err := bc.openRequest(ctx, api.TWatch, api.WatchRequest{RequestID: requestID})
	if err != nil {
		return api.Snapshot{}, err
	}
	defer stop()
	defer amberclient.CloseStream(stream)
	rt, rb, err := api.ReadFrame(stream)
	if err != nil {
		return api.Snapshot{}, fmt.Errorf("watch stream: %w", err)
	}
	var snap api.Snapshot
	err = decodeReply(rt, rb, api.TSnapshot, &snap)
	return snap, err
}

// runBuildTUI decides on and runs the build view for one request. handled
// is false when the caller should fall back to the classic block view (old
// server without graph edges, or a first snapshot that is already
// terminal — the block path resolves those instantly with today's UX).
func runBuildTUI(ctx context.Context, c *cli.Context, bc *apiConn, requestID, label string, lv *liveView) (handled bool, out tui.BuildOutcome, err error) {
	snap, err := peekSnapshot(ctx, bc, requestID)
	if err != nil {
		return true, tui.BuildOutcome{}, err
	}
	if snap.Terminal {
		return false, tui.BuildOutcome{}, nil
	}
	if !tui.SnapshotHasGraph(snap) {
		lv.Println("server predates graph snapshots; using the classic view")
		return false, tui.BuildOutcome{}, nil
	}
	// The view owns ctrl-c (confirm-cancel); SIGTERM lands in this context
	// and reads as detach.
	tctx, tstop := signalCtx(ctx)
	defer tstop()
	out, err = tui.RunBuildWatch(tctx, &buildStreams{bc: bc, reqID: requestID}, tui.BuildWatchOptions{
		RequestID: requestID,
		Label:     label,
	})
	return true, out, err
}
