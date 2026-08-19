package tui

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"
)

// tn builds a syntactically valid node name for tests: kind_<64-hex>.
func tn(kind string, i int) string { return fmt.Sprintf("%s_%064x", kind, i) }

// chainSnap builds the canonical single-build closure: buildvalue 1 with a
// full stage chain (buildfrom 1, pluginresolve 2, pin 2, buildrun 3) and a
// source import 4 under the buildfrom. Callers mutate phases per case.
func chainSnap() api.Snapshot {
	bv, bf := tn(wire.KindBuildValue, 1), tn(wire.KindBuildFrom, 1)
	pr, pin := tn(wire.KindPluginResolve, 2), tn(wire.KindPin, 2)
	br, imp := tn(wire.KindBuildRun, 3), tn(wire.KindImport, 4)
	return api.Snapshot{
		RequestID: "r1",
		Phase:     "running",
		Nodes: []api.NodeSnap{
			{Node: bv, Label: "app", Phase: wire.PhaseWaiting, Deps: []string{bf, pr, pin, br}},
			{Node: bf, Phase: wire.PhaseDone, ElapsedMs: 1200, Deps: []string{imp}},
			{Node: pr, Phase: wire.PhaseDone, ElapsedMs: 300},
			{Node: pin, Phase: wire.PhaseDone, ElapsedMs: 700},
			{Node: br, Phase: wire.PhaseRunning, ElapsedMs: 9000, Runner: "r-a"},
			{Node: imp, Label: "fetch github", Phase: wire.PhaseDone, ElapsedMs: 2000},
		},
	}
}

func row(t *testing.T, g *buildGraph, name string) *buildRow {
	t.Helper()
	r, ok := g.rows[name]
	if !ok {
		t.Fatalf("row %s missing (rows: %v)", name, len(g.rows))
	}
	return r
}

func TestFoldChainRunning(t *testing.T) {
	snap := chainSnap()
	g := foldSnapshot(snap)

	bv := tn(wire.KindBuildValue, 1)
	if !reflect.DeepEqual(g.roots, []string{bv}) {
		t.Fatalf("roots = %v, want [%s]", g.roots, bv)
	}
	r := row(t, g, bv)
	if r.Phase != wire.PhaseRunning || r.Stage != "build" {
		t.Fatalf("row phase/stage = %s/%s, want running/build", r.Phase, r.Stage)
	}
	if r.ElapsedMs != 9000 || r.Runner != "r-a" {
		t.Fatalf("elapsed/runner = %d/%s, want 9000/r-a", r.ElapsedMs, r.Runner)
	}
	if r.LogNode != tn(wire.KindBuildRun, 3) {
		t.Fatalf("log node = %s, want the running buildrun", r.LogNode)
	}
	if !reflect.DeepEqual(r.Children, []string{tn(wire.KindImport, 4)}) {
		t.Fatalf("children = %v, want the source import", r.Children)
	}
	imp := row(t, g, tn(wire.KindImport, 4))
	if imp.Phase != wire.PhaseDone || imp.Stage != "fetch" || imp.ElapsedMs != 2000 {
		t.Fatalf("import row = %+v", imp)
	}
	if imp.LogNode != imp.Node {
		t.Fatalf("import log node = %s, want itself", imp.LogNode)
	}
}

func TestFoldChainFailed(t *testing.T) {
	snap := chainSnap()
	// pin failed; buildrun never registered (chain stops at the failure).
	snap.Nodes[3].Phase, snap.Nodes[3].ErrSummary = wire.PhaseFailed, "pin exploded"
	snap.Nodes[4].Phase = wire.PhaseWaiting
	snap.Nodes[0].Phase = wire.PhaseUpstream
	g := foldSnapshot(snap)

	r := row(t, g, tn(wire.KindBuildValue, 1))
	if r.Phase != wire.PhaseFailed || r.Stage != "pin" || r.Err != "pin exploded" {
		t.Fatalf("row = %+v, want failed/pin/pin exploded", r)
	}
	if r.LogNode != tn(wire.KindPin, 2) {
		t.Fatalf("log node = %s, want the failed pin", r.LogNode)
	}
}

func TestFoldUpstreamVerdictWhenChainClean(t *testing.T) {
	// The failure lives in a child build: the parent's own chain is clean
	// but the server derived failed-upstream for it.
	snap := chainSnap()
	snap.Nodes[0].Phase = wire.PhaseUpstream
	snap.Nodes[4].Phase = wire.PhaseWaiting // buildrun blocked, not failed
	g := foldSnapshot(snap)
	r := row(t, g, tn(wire.KindBuildValue, 1))
	if r.Phase != wire.PhaseUpstream {
		t.Fatalf("phase = %s, want %s", r.Phase, wire.PhaseUpstream)
	}
}

func TestFoldDoneDurationFromBuildrun(t *testing.T) {
	snap := chainSnap()
	snap.Nodes[0].Phase = wire.PhaseDone
	snap.Nodes[4].Phase = wire.PhaseRunning // stale phase must lose to… no:
	// a done buildvalue with a still-running chain doesn't happen; make the
	// whole chain done for the real case.
	snap.Nodes[4].Phase, snap.Nodes[4].ElapsedMs = wire.PhaseDone, 41000
	g := foldSnapshot(snap)
	r := row(t, g, tn(wire.KindBuildValue, 1))
	if r.Phase != wire.PhaseDone || r.ElapsedMs != 41000 || r.Cached {
		t.Fatalf("row = %+v, want done/41000/uncached", r)
	}
	if r.LogNode != tn(wire.KindBuildRun, 3) {
		t.Fatalf("done row log node = %s, want the buildrun", r.LogNode)
	}
}

func TestFoldCached(t *testing.T) {
	// Fast-pathed target: a lone cached buildvalue, no chain at all.
	bv := tn(wire.KindBuildValue, 1)
	g := foldSnapshot(api.Snapshot{Nodes: []api.NodeSnap{
		{Node: bv, Phase: wire.PhaseDone, Cached: true},
	}})
	r := row(t, g, bv)
	if r.Phase != wire.PhaseDone || !r.Cached || r.LogNode != "" {
		t.Fatalf("row = %+v, want done/cached/no log node", r)
	}

	// KP-memo hit: the chain ran up to pin, the buildrun was fast-pathed.
	snap := chainSnap()
	snap.Nodes[0].Phase = wire.PhaseDone
	snap.Nodes[4].Phase, snap.Nodes[4].Cached, snap.Nodes[4].ElapsedMs = wire.PhaseDone, true, 0
	g = foldSnapshot(snap)
	if r := row(t, g, bv); !r.Cached {
		t.Fatalf("KP-memo row not cached: %+v", r)
	}
}

func TestFoldSharedDepUnderBothParents(t *testing.T) {
	// Two buildvalues' buildruns both dep on one shared child buildvalue.
	bv1, br1 := tn(wire.KindBuildValue, 1), tn(wire.KindBuildRun, 11)
	bv2, br2 := tn(wire.KindBuildValue, 2), tn(wire.KindBuildRun, 12)
	child := tn(wire.KindBuildValue, 3)
	root, rootRun := tn(wire.KindBuildValue, 9), tn(wire.KindBuildRun, 19)
	g := foldSnapshot(api.Snapshot{Nodes: []api.NodeSnap{
		{Node: root, Label: "root", Phase: wire.PhaseWaiting, Deps: []string{rootRun}},
		{Node: rootRun, Phase: wire.PhaseWaiting, Deps: []string{bv1, bv2}},
		{Node: bv1, Label: "a", Phase: wire.PhaseWaiting, Deps: []string{br1}},
		{Node: br1, Phase: wire.PhaseWaiting, Deps: []string{child}},
		{Node: bv2, Label: "b", Phase: wire.PhaseWaiting, Deps: []string{br2}},
		{Node: br2, Phase: wire.PhaseWaiting, Deps: []string{child}},
		{Node: child, Label: "shared", Phase: wire.PhaseDone, Cached: true},
	}})
	if !reflect.DeepEqual(g.roots, []string{root}) {
		t.Fatalf("roots = %v, want [%s]", g.roots, root)
	}
	if got := row(t, g, bv1).Children; !reflect.DeepEqual(got, []string{child}) {
		t.Fatalf("bv1 children = %v", got)
	}
	if got := row(t, g, bv2).Children; !reflect.DeepEqual(got, []string{child}) {
		t.Fatalf("bv2 children = %v", got)
	}
	// Root's children ordered by label: a before b.
	if got := row(t, g, root).Children; !reflect.DeepEqual(got, []string{bv1, bv2}) {
		t.Fatalf("root children = %v, want [a b]", got)
	}

	// The shared subtree repeats under both parents when expanded.
	rows := flattenTree(g, map[string]bool{})
	var childPaths []string
	for _, tr := range rows {
		if tr.Node == child {
			childPaths = append(childPaths, tr.Path)
		}
	}
	if len(childPaths) != 2 {
		t.Fatalf("shared child appears %d times, want 2 (%v)", len(childPaths), childPaths)
	}
}

func TestFoldImportFetcherEdge(t *testing.T) {
	imp, fbv := tn(wire.KindImport, 1), tn(wire.KindBuildValue, 2)
	fbr := tn(wire.KindBuildRun, 3)
	g := foldSnapshot(api.Snapshot{Nodes: []api.NodeSnap{
		{Node: imp, Label: "fetch github", Phase: wire.PhaseWaiting, Deps: []string{fbv}},
		{Node: fbv, Label: "fetcher github", Phase: wire.PhaseRunning, Deps: []string{fbr}},
		{Node: fbr, Phase: wire.PhaseRunning, ElapsedMs: 100},
	}})
	if got := row(t, g, imp).Children; !reflect.DeepEqual(got, []string{fbv}) {
		t.Fatalf("import children = %v, want the fetcher buildvalue", got)
	}
	if !reflect.DeepEqual(g.roots, []string{imp}) {
		t.Fatalf("roots = %v", g.roots)
	}
}

func TestSnapshotHasGraph(t *testing.T) {
	multiNoDeps := api.Snapshot{Nodes: []api.NodeSnap{
		{Node: tn(wire.KindBuildValue, 1), Phase: wire.PhaseWaiting},
		{Node: tn(wire.KindBuildFrom, 1), Phase: wire.PhaseRunning},
	}}
	if SnapshotHasGraph(multiNoDeps) {
		t.Fatal("old-server snapshot (multi node, no deps) reported a graph")
	}
	single := api.Snapshot{Nodes: []api.NodeSnap{{Node: tn(wire.KindBuildValue, 1), Phase: wire.PhaseDone, Cached: true}}}
	if !SnapshotHasGraph(single) {
		t.Fatal("single-node snapshot should fold fine without deps")
	}
	if !SnapshotHasGraph(chainSnap()) {
		t.Fatal("deps-bearing snapshot reported no graph")
	}
}

func TestFlattenExpansion(t *testing.T) {
	snap := chainSnap()
	// Import gains a fetcher buildvalue child so depth-2 exists; the import
	// itself is cached-done → collapsed by default.
	fbv := tn(wire.KindBuildValue, 5)
	snap.Nodes[5].Cached = true
	snap.Nodes[5].Deps = []string{fbv}
	snap.Nodes = append(snap.Nodes, api.NodeSnap{Node: fbv, Phase: wire.PhaseDone, Cached: true})
	g := foldSnapshot(snap)

	exp := map[string]bool{}
	rows := flattenTree(g, exp)
	// Default: root expanded (running), import visible but collapsed
	// (cached-done) → 2 rows.
	if len(rows) != 2 {
		t.Fatalf("default flatten = %d rows (%+v), want 2", len(rows), rows)
	}
	if rows[1].Node != tn(wire.KindImport, 4) || rows[1].Expanded || !rows[1].HasKids {
		t.Fatalf("import row = %+v, want collapsed with kids", rows[1])
	}

	// User expands the import path → the fetcher appears.
	exp[rows[1].Path] = true
	rows = flattenTree(g, exp)
	if len(rows) != 3 || rows[2].Node != fbv || rows[2].Depth != 2 {
		t.Fatalf("expanded flatten = %+v, want fetcher at depth 2", rows)
	}

	// User collapses the root → only the root remains.
	exp[rows[0].Path] = false
	rows = flattenTree(g, exp)
	if len(rows) != 1 || rows[0].Node != tn(wire.KindBuildValue, 1) {
		t.Fatalf("collapsed flatten = %+v, want just the root", rows)
	}
}

func TestFlattenCycleGuard(t *testing.T) {
	// A malformed snapshot with a logical cycle must not recurse forever.
	a, ab := tn(wire.KindBuildValue, 1), tn(wire.KindBuildRun, 1)
	b, bb := tn(wire.KindBuildValue, 2), tn(wire.KindBuildRun, 2)
	g := foldSnapshot(api.Snapshot{Nodes: []api.NodeSnap{
		{Node: a, Phase: wire.PhaseWaiting, Deps: []string{ab}},
		{Node: ab, Phase: wire.PhaseWaiting, Deps: []string{b}},
		{Node: b, Phase: wire.PhaseWaiting, Deps: []string{bb}},
		{Node: bb, Phase: wire.PhaseWaiting, Deps: []string{a}},
	}})
	rows := flattenTree(g, map[string]bool{})
	if len(rows) == 0 || len(rows) > 4 {
		t.Fatalf("cycle flatten = %d rows", len(rows))
	}
}
