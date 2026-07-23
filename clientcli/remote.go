package clientcli

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/fables-for-robots/amber-store-core/key"
	"github.com/urfave/cli/v2"

	"github.com/fables-for-robots/jobs-iroh/amberclient"
	"github.com/fables-for-robots/jobs-iroh/api"
	"github.com/fables-for-robots/jobs-iroh/builddef"
	"github.com/fables-for-robots/jobs-iroh/runner"
	"github.com/fables-for-robots/jobs-iroh/wire"
)

// treeDefinition constructs the canonical build Definition for a pushed
// source tree: a tree-source Input over the ingested source root, with the
// same dir/params/platform/build-file knobs the local path feeds
// localBuildFrom. IngestSourceDir + these fields are the whole identity —
// the server's buildfrom stage resolves the same env subtree, the same
// recipe override and the same params file, so the remote F equals the F a
// local build of the same source computes (the cache-join invariant).
func treeDefinition(sourceKey key.Key, dir, buildFile, platform string, params []byte) (canon []byte, k key.Key, err error) {
	in, err := builddef.TreeInput(sourceKey)
	if err != nil {
		return nil, key.Key{}, err
	}
	def := builddef.Definition{
		Source:    in,
		Dir:       dir,
		Platform:  platform,
		Params:    params,
		BuildFile: buildFile,
	}
	canon, err = def.Canonical()
	if err != nil {
		return nil, key.Key{}, fmt.Errorf("canonicalize definition: %w", err)
	}
	k, err = def.Key()
	if err != nil {
		return nil, key.Key{}, fmt.Errorf("definition key: %w", err)
	}
	return canon, k, nil
}

// remoteConfig carries the remote-build flags: where the server is, what to
// build, and the optional resource raise for the target build.
type remoteConfig struct {
	server    string
	dataDir   string
	source    string
	dir       string
	buildFile string
	platform  string
	cpu       string
	memory    string
}

func remoteBuildCmd() *cli.Command {
	cfg := &remoteConfig{}
	return &cli.Command{
		Name:  "remote-build",
		Usage: "push a source tree to a jobs-server, build it there, pull the output home",
		Flags: []cli.Flag{
			&cli.StringFlag{Name: "server", Required: true, EnvVars: []string{"JOBS_SERVER"}, Usage: "endpoint ID of the jobs-server", Destination: &cfg.server},
			&cli.StringSliceFlag{Name: "addr", EnvVars: []string{"JOBS_SERVER_ADDR"}, Usage: "direct server address host:port (repeatable; skips discovery and relays)"},
			&cli.StringFlag{Name: "data-dir", EnvVars: []string{"JOBS_DATA_DIR"}, Value: defaultDataDir(), Usage: "client data directory (embedded store + cache)", Destination: &cfg.dataDir},
			&cli.StringFlag{Name: "source", Required: true, Usage: "source directory to ingest and push as the build source", Destination: &cfg.source},
			&cli.StringFlag{Name: "dir", Usage: "build root within the source (where BUILD.jobs lives)", Destination: &cfg.dir},
			&cli.StringFlag{Name: "build-file", Usage: "recipe path relative to dir (default BUILD.jobs)", Destination: &cfg.buildFile},
			&cli.StringFlag{Name: "platform", EnvVars: []string{"JOBS_PLATFORM"}, Value: runner.Platform(), Usage: "target platform, e.g. linux/amd64", Destination: &cfg.platform},
			&cli.StringSliceFlag{Name: "param", Usage: "key=value build param (repeatable)"},
			&cli.StringFlag{Name: "cpu", Usage: "raise the target build's CPU requirement (e.g. 2000m)", Destination: &cfg.cpu},
			&cli.StringFlag{Name: "memory", Usage: "raise the target build's memory requirement (e.g. 4Gi)", Destination: &cfg.memory},
		},
		Action: cfg.run,
	}
}

// run drives the four remote-build phases (jobs remote-build's shape): ingest
// + push the source tree, submit the canonical definition, watch snapshots to
// terminal (SIGINT sends a cancel frame first), then pull the output home.
func (cfg *remoteConfig) run(c *cli.Context) error {
	ctx, cancel := context.WithCancel(c.Context)
	defer cancel()
	ew := errWriter(c)
	addrs := c.StringSlice("addr")

	params, err := parseParams(c.StringSlice("param"))
	if err != nil {
		return err
	}

	// SHARED lock: push/pull only ever add objects and write fresh ref
	// names — no local build mutates shared state under us.
	cs, err := openClientStore(cfg.dataDir, lockShared)
	if err != nil {
		return err
	}
	defer cs.Close()

	fmt.Fprintf(ew, "ingesting %s\n", cfg.source)
	sourceKey, err := cs.Store.IngestSourceDir(ctx, cfg.source)
	if err != nil {
		return fmt.Errorf("ingest source %s: %w", cfg.source, err)
	}
	canon, k, err := treeDefinition(sourceKey, cfg.dir, cfg.buildFile, cfg.platform, params)
	if err != nil {
		return err
	}

	ac, err := amberclient.Dial(ctx, amberclient.Options{
		EndpointID: cfg.server, Addrs: addrs, ALPN: alpnAmberAdmin,
	})
	if err != nil {
		return err
	}
	defer ac.Close()

	scratch, err := clientPushRef()
	if err != nil {
		return err
	}
	fmt.Fprintf(ew, "pushing source tree %s\n", sourceKey)
	if err := ac.Push(ctx, cs.Store, scratch, sourceKey); err != nil {
		return err
	}

	bc, err := dialAPI(ctx, cfg.server, addrs, alpnBuild)
	if err != nil {
		return err
	}
	defer bc.Close()

	var res *api.ResourceSpec
	if cfg.cpu != "" || cfg.memory != "" {
		res = &api.ResourceSpec{CPU: cfg.cpu, Memory: cfg.memory}
	}
	var sub api.Submitted
	err = bc.call(ctx, api.TSubmit, api.SubmitRequest{
		Def:        canon,
		Resources:  res,
		ScratchRef: scratch,
	}, api.TSubmitted, &sub)
	if err != nil {
		return err
	}
	fmt.Fprintf(ew, "submitted request %s (build %s)\n", sub.RequestID, k)

	// SIGINT/SIGTERM: send the cancel frame on its own stream, then let the
	// watch loop unwind via ctx so every deferred teardown still runs. A
	// second signal falls back to the default handler (kill).
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(sig)
	go func() {
		if _, ok := <-sig; !ok {
			return
		}
		signal.Stop(sig)
		fmt.Fprintln(ew, "interrupt: cancelling build")
		cctx, cdone := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cdone()
		if err := bc.call(cctx, api.TCancel, api.CancelRequest{RequestID: sub.RequestID}, api.TOK, nil); err != nil {
			fmt.Fprintln(ew, "cancel failed:", err)
		}
		cancel()
	}()

	final, err := streamWatch(ctx, bc, sub.RequestID, ew)
	if err != nil {
		if ctx.Err() != nil {
			return cli.Exit("build cancelled", 130)
		}
		return err
	}

	switch final.Phase {
	case "done":
		return cfg.pullHome(ctx, c, ac, cs, k)
	case "failed":
		printFailureLogs(ctx, bc, final, ew)
		return fmt.Errorf("build %s failed", sub.RequestID)
	default: // cancelled
		return cli.Exit("build cancelled", 130)
	}
}

// pullHome fetches the finished build's refs into the local store —
// build-from:K first (K→F), then build-output:F and build-output-deps:F —
// and prints the identity and output key like a local build.
func (cfg *remoteConfig) pullHome(ctx context.Context, c *cli.Context, ac *amberclient.Client, cs *clientStore, k key.Key) error {
	f, err := ac.Pull(ctx, cs.Store, "build-from:"+k.String())
	if err != nil {
		return fmt.Errorf("pull build-from: %w", err)
	}
	outKey, err := ac.Pull(ctx, cs.Store, "build-output:"+f.String())
	if err != nil {
		return fmt.Errorf("pull build-output: %w", err)
	}
	if _, err := ac.Pull(ctx, cs.Store, "build-output-deps:"+f.String()); err != nil &&
		!errors.Is(err, amberclient.ErrRefNotFound) {
		return fmt.Errorf("pull build-output-deps: %w", err)
	}
	fmt.Fprintf(c.App.Writer, "build:  %s\n", f.String())
	fmt.Fprintf(c.App.Writer, "output: %s\n", outKey.String())
	return nil
}

// streamWatch opens a watch stream and renders coalesced snapshots one line
// per change until the terminal snapshot, which it returns.
func streamWatch(ctx context.Context, bc *apiConn, requestID string, w io.Writer) (api.Snapshot, error) {
	stream, stop, err := bc.openRequest(ctx, api.TWatch, api.WatchRequest{RequestID: requestID})
	if err != nil {
		return api.Snapshot{}, err
	}
	defer stop()
	defer stream.Close()

	var last string
	for {
		rt, body, err := api.ReadFrame(stream)
		if err != nil {
			if ctx.Err() != nil {
				return api.Snapshot{}, ctx.Err()
			}
			return api.Snapshot{}, fmt.Errorf("watch stream: %w", err)
		}
		var snap api.Snapshot
		if err := decodeReply(rt, body, api.TSnapshot, &snap); err != nil {
			return api.Snapshot{}, err
		}
		renderSnapshot(w, snap, &last)
		if snap.Terminal {
			return snap, nil
		}
	}
}

// renderSnapshot prints one status line — phase, counts, and the nodes
// currently in flight — deduplicated against the previously printed line.
func renderSnapshot(w io.Writer, snap api.Snapshot, last *string) {
	var active []string
	for _, n := range snap.Nodes {
		switch n.Phase {
		case wire.PhaseQueued, wire.PhaseRunning, wire.PhasePublishing:
			active = append(active, shortNode(n.Node))
		}
	}
	const maxShown = 6
	if len(active) > maxShown {
		active = append(active[:maxShown], "…")
	}
	line := fmt.Sprintf("%-9s done %d/%d  running %d  failed %d",
		snap.Phase, snap.Counts.Done, snap.Counts.Total, snap.Counts.Running, snap.Counts.Failed)
	if len(active) > 0 {
		line += "  [" + strings.Join(active, " ") + "]"
	}
	if line != *last {
		fmt.Fprintln(w, line)
		*last = line
	}
}

// shortNode renders a node name as kind:keyprefix for one-line status output.
func shortNode(name string) string {
	kind, k, err := wire.ParseNodeName(name)
	if err != nil {
		return name
	}
	return kind + ":" + k.String()[:8]
}

// printFailureLogs fetches and prints the captured output of the terminal
// snapshot's failing nodes (the hard failures, not the derived
// failed-upstream ones). Best-effort: log fetch problems are reported and
// swallowed — the build error is the story.
func printFailureLogs(ctx context.Context, bc *apiConn, snap api.Snapshot, w io.Writer) {
	for _, n := range snap.Nodes {
		if n.Phase != wire.PhaseFailed {
			continue
		}
		fmt.Fprintf(w, "\n--- %s failed (gen %d): %s\n", n.Node, n.Gen, n.ErrSummary)
		var view api.LogView
		if err := bc.call(ctx, api.TLogs, api.LogsRequest{Node: n.Node}, api.TLogView, &view); err != nil {
			fmt.Fprintln(w, "(logs unavailable:", err, ")")
			continue
		}
		if len(view.Head) == 0 && len(view.Tail) == 0 {
			fmt.Fprintln(w, "(no captured output)")
			continue
		}
		w.Write(view.Head)
		if view.GapSize > 0 {
			fmt.Fprintf(w, "\n... [%d bytes omitted] ...\n", view.GapSize)
		}
		w.Write(view.Tail)
		fmt.Fprintln(w)
	}
}

// clientPushRef mints the scratch ref name for one source push; the server
// deletes it after the submit commits.
func clientPushRef() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "client-push/" + hex.EncodeToString(b[:]), nil
}

// errWriter is the CLI's stderr (App.ErrWriter when set, so tests can
// capture progress output).
func errWriter(c *cli.Context) io.Writer {
	if c.App.ErrWriter != nil {
		return c.App.ErrWriter
	}
	return os.Stderr
}
