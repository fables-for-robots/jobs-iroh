package clientcli

import (
	"context"
	"errors"
	"net/netip"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/serve"
	"github.com/jobs-build/jobs-iroh/tui"
)

// TestBuildStreamsAgainstServer exercises the build view's transport seam
// against a real server over loopback iroh: the peek (terminal fast-path
// snapshot, graph detection) and the ServerError typing the view's
// retry-vs-fatal split depends on. No runner — the pre-seeded doneness
// fast-path finishes the request at submit.
func TestBuildStreamsAgainstServer(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	ready := make(chan *serve.Server, 1)
	done := make(chan error, 1)
	srvCtx, stopSrv := context.WithCancel(ctx)
	t.Cleanup(func() {
		stopSrv()
		select {
		case err := <-done:
			if err != nil {
				t.Errorf("server run: %v", err)
			}
		case <-time.After(30 * time.Second):
			t.Error("server did not shut down")
		}
	})
	go func() {
		done <- serve.Run(srvCtx, serve.Options{
			DataDir:  t.TempDir(),
			BindAddr: netip.AddrPortFrom(netip.IPv6Loopback(), 0),
			Ready:    func(s *serve.Server) { ready <- s },
		})
	}()
	var srv *serve.Server
	select {
	case srv = <-ready:
	case <-time.After(30 * time.Second):
		t.Fatal("server not ready")
	}

	bc, err := dialAPI(ctx, srv.Endpoint.ID().String(), []string{srv.Endpoint.LocalAddr().String()}, alpnBuild)
	if err != nil {
		t.Fatal(err)
	}
	defer bc.Close()

	// A watch on an unknown request answers an api.Error frame — the seam
	// must surface it as *api.ServerError (the view quits instead of
	// retrying forever).
	_, err = peekSnapshot(ctx, bc, "no-such-request")
	var se *api.ServerError
	if !errors.As(err, &se) {
		t.Fatalf("peek unknown request: err = %v, want *api.ServerError", err)
	}

	// Pre-seeded fast-path build: submit completes instantly; the peek sees
	// the terminal snapshot (remote-build then skips the TUI) and the
	// single-node closure still counts as a graph.
	srcDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(srcDir, "BUILD.jobs"), []byte("# recipe\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	treeKey, err := srv.Store.IngestSourceDir(ctx, srcDir)
	if err != nil {
		t.Fatal(err)
	}
	canon, k, err := treeDefinition(treeKey, "", "", "linux/amd64", nil)
	if err != nil {
		t.Fatal(err)
	}
	outDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(outDir, "artifact"), []byte("built!"), 0o644); err != nil {
		t.Fatal(err)
	}
	outKey, err := srv.Store.IngestDir(ctx, outDir)
	if err != nil {
		t.Fatal(err)
	}
	f := treeKey // stand-in F
	if err := srv.Store.PutRef(ctx, "build-from:"+k.String(), f); err != nil {
		t.Fatal(err)
	}
	if err := srv.Store.PutRef(ctx, "build-output:"+f.String(), outKey); err != nil {
		t.Fatal(err)
	}

	var sub api.Submitted
	if err := bc.call(ctx, api.TSubmit, api.SubmitRequest{Def: canon}, api.TSubmitted, &sub); err != nil {
		t.Fatalf("submit: %v", err)
	}
	snap, err := peekSnapshot(ctx, bc, sub.RequestID)
	if err != nil {
		t.Fatalf("peek: %v", err)
	}
	if !snap.Terminal || snap.Phase != "done" {
		t.Fatalf("peek snapshot = phase %s terminal %v, want done/terminal", snap.Phase, snap.Terminal)
	}
	if !tui.SnapshotHasGraph(snap) {
		t.Fatalf("single-node terminal snapshot must count as a graph: %+v", snap.Nodes)
	}
	if len(snap.Nodes) != 1 || !snap.Nodes[0].Cached {
		t.Fatalf("fast-pathed closure = %+v, want one cached node", snap.Nodes)
	}

	// The streams seam end-to-end: OpenWatch replays the terminal snapshot.
	bs := &buildStreams{bc: bc, reqID: sub.RequestID}
	next, stop, err := bs.OpenWatch(ctx)
	if err != nil {
		t.Fatalf("OpenWatch: %v", err)
	}
	defer stop()
	got, err := next()
	if err != nil {
		t.Fatalf("watch next: %v", err)
	}
	if !got.Terminal || got.Phase != "done" {
		t.Fatalf("watched snapshot = %s/%v", got.Phase, got.Terminal)
	}

	// OpenLogs on the closure's node: empty stored view, no error.
	view, _, logStop, err := bs.OpenLogs(ctx, snap.Nodes[0].Node)
	if err != nil {
		t.Fatalf("OpenLogs: %v", err)
	}
	logStop()
	if len(view.Head) != 0 || len(view.Tail) != 0 {
		t.Fatalf("unexpected stored output: %+v", view)
	}
}
