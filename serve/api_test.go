package serve

// Build/admin ALPN tests: the frame protocol end to end over loopback iroh
// against the real scheduler — no runner, so requests stay in flight (their
// jobs sit on the work queue) until cancelled.

import (
	"context"
	"encoding/hex"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tmc/go-iroh/iroh"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/builddef"
	"github.com/jobs-build/jobs-iroh/importdef"
)

// request performs one framed request on a fresh stream and returns the
// first reply frame, decoded into reply (which may be nil).
func request(t *testing.T, ctx context.Context, conn *iroh.Conn, typ string, body any, wantType string, reply any) {
	t.Helper()
	stream := openRequest(t, ctx, conn, typ, body)
	defer stream.Close()
	readReply(t, stream, wantType, reply)
}

func openRequest(t *testing.T, ctx context.Context, conn *iroh.Conn, typ string, body any) net.Conn {
	t.Helper()
	stream, err := conn.OpenStreamConn(ctx)
	if err != nil {
		t.Fatalf("open stream: %v", err)
	}
	stream.SetDeadline(time.Now().Add(30 * time.Second))
	if err := api.WriteFrame(stream, typ, body); err != nil {
		t.Fatalf("write %s: %v", typ, err)
	}
	return stream
}

func readReply(t *testing.T, stream net.Conn, wantType string, reply any) {
	t.Helper()
	rt, rb, err := api.ReadFrame(stream)
	if err != nil {
		t.Fatalf("read reply (want %s): %v", wantType, err)
	}
	if rt == api.TError && wantType != api.TError {
		var e api.Error
		_ = api.DecodeBody(rb, &e)
		t.Fatalf("server error: %s: %s (want %s)", e.Code, e.Text, wantType)
	}
	if rt != wantType {
		t.Fatalf("reply frame %q, want %q", rt, wantType)
	}
	if reply != nil {
		if err := api.DecodeBody(rb, reply); err != nil {
			t.Fatalf("decode %s: %v", wantType, err)
		}
	}
}

func TestBuildAndAdminAPI(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	srv, dial := startServer(t, ctx)

	// A source tree in the server store, simulating the client's
	// jobs-amber-admin push that precedes every tree-source submit.
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD.jobs"), []byte("# recipe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	treeKey, err := srv.Store.IngestSourceDir(ctx, srcDir)
	if err != nil {
		t.Fatalf("ingest source: %v", err)
	}
	in, err := builddef.TreeInput(treeKey)
	if err != nil {
		t.Fatal(err)
	}
	params, err := importdef.CanonicalParams(map[string]any{})
	if err != nil {
		t.Fatal(err)
	}
	def := builddef.Definition{Source: in, Platform: "linux/amd64", Params: params}
	canon, err := def.Canonical()
	if err != nil {
		t.Fatal(err)
	}
	k, err := def.Key()
	if err != nil {
		t.Fatal(err)
	}

	build := dial(ALPNBuild)
	admin := dial(ALPNAdmin)

	// Submit over the build ALPN.
	var sub api.Submitted
	request(t, ctx, build, api.TSubmit, api.SubmitRequest{Def: canon}, api.TSubmitted, &sub)
	if sub.RequestID == "" {
		t.Fatal("empty request id")
	}
	if hex.EncodeToString(sub.K) != k.String() {
		t.Fatalf("submitted K %x, want %s", sub.K, k)
	}

	// Watch: the first snapshot arrives immediately and shows a live
	// closure (buildvalue + buildfrom at least).
	ws := openRequest(t, ctx, build, api.TWatch, api.WatchRequest{RequestID: sub.RequestID})
	var snap api.Snapshot
	readReply(t, ws, api.TSnapshot, &snap)
	ws.Close()
	if snap.Phase != "running" || snap.Counts.Total < 2 {
		t.Fatalf("first snapshot: phase=%q counts=%+v", snap.Phase, snap.Counts)
	}

	// Admin: requests lists the live request.
	var rr api.RequestsReply
	request(t, ctx, admin, api.TRequests, nil, api.TRequestsReply, &rr)
	if len(rr.Requests) != 1 || rr.Requests[0].RequestID != sub.RequestID {
		t.Fatalf("requests listing: %+v", rr.Requests)
	}

	// Admin: fleet decodes (no runners connected).
	var fl api.FleetReply
	request(t, ctx, admin, api.TFleet, nil, api.TFleetReply, &fl)
	if len(fl.Runners) != 0 {
		t.Fatalf("unexpected runners: %+v", fl.Runners)
	}

	// Admin: stats reflect the store on disk and the tracked graph.
	var st api.StatsReply
	request(t, ctx, admin, api.TStats, nil, api.TStatsReply, &st)
	if st.StoreBytes <= 0 || st.RefCount == 0 || st.Requests != 1 || st.NodesTracked < 2 {
		t.Fatalf("stats: %+v", st)
	}

	// Admin: refs with a prefix filter finds the submit's bookkeeping ref.
	var refs api.RefsReply
	request(t, ctx, admin, api.TRefs, api.RefsRequest{Prefix: "build:"}, api.TRefsReply, &refs)
	if len(refs.Refs) != 1 || refs.Refs[0].Name != "build:"+k.String() {
		t.Fatalf("refs listing: %+v", refs.Refs)
	}

	// Diagnose over the admin ALPN: a node that never failed has no records
	// (proves the FAILURES stream + dispatch wiring, not the trail — the
	// sched tests own that).
	var de api.Error
	request(t, ctx, admin, api.TDiagnose,
		api.DiagnoseRequest{Node: "buildfrom_" + k.String()}, api.TError, &de)
	if de.Code != api.CodeNotFound {
		t.Fatalf("diagnose of an unfailed node: %+v", de)
	}

	// Logs for a node that produced no output yet: an empty view, not an
	// error.
	var view api.LogView
	nodeName := "buildfrom_" + k.String()
	request(t, ctx, build, api.TLogs, api.LogsRequest{Node: nodeName}, api.TLogView, &view)
	if view.Node != nodeName || len(view.Head) != 0 {
		t.Fatalf("log view: %+v", view)
	}

	// Cancel on the build ALPN, then the watch stream reaches terminal.
	request(t, ctx, build, api.TCancel, api.CancelRequest{RequestID: sub.RequestID}, api.TOK, nil)
	ws = openRequest(t, ctx, build, api.TWatch, api.WatchRequest{RequestID: sub.RequestID})
	readReply(t, ws, api.TSnapshot, &snap)
	ws.Close()
	if !snap.Terminal || snap.Phase != "cancelled" {
		t.Fatalf("post-cancel snapshot: %+v", snap)
	}

	// Delete via admin, after which the request is gone. (Fresh reply value:
	// an empty listing is an omitempty-empty body that would leave a reused
	// variable's old slice in place.)
	request(t, ctx, admin, api.TDelete, api.CancelRequest{RequestID: sub.RequestID}, api.TOK, nil)
	var after api.RequestsReply
	request(t, ctx, admin, api.TRequests, nil, api.TRequestsReply, &after)
	if len(after.Requests) != 0 {
		t.Fatalf("requests after delete: %+v", after.Requests)
	}
}

func TestBuildALPNRejections(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	_, dial := startServer(t, ctx)
	build := dial(ALPNBuild)

	// Admin-only frames are refused on the build ALPN.
	var e api.Error
	request(t, ctx, build, api.TStats, nil, api.TError, &e)
	if e.Code != api.CodeBadRequest {
		t.Fatalf("stats on build ALPN: %+v", e)
	}
	request(t, ctx, build, api.TDiagnose, api.DiagnoseRequest{Node: "x"}, api.TError, &e)
	if e.Code != api.CodeBadRequest {
		t.Fatalf("diagnose on build ALPN: %+v", e)
	}

	// A non-canonical definition is a bad request.
	request(t, ctx, build, api.TSubmit, api.SubmitRequest{Def: []byte{0xa0}}, api.TError, &e)
	if e.Code != api.CodeBadRequest {
		t.Fatalf("bogus submit: %+v", e)
	}

	// Watching an unknown request is not-found.
	request(t, ctx, build, api.TWatch, api.WatchRequest{RequestID: "nope"}, api.TError, &e)
	if e.Code != api.CodeNotFound {
		t.Fatalf("unknown watch: %+v", e)
	}
}
