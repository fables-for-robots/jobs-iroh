package tui

// Build-graph folding for the build view (docs/design/2026-08-19-remote-
// build-tui.md §2): the snapshot's raw scheduler nodes fold into logical
// rows — one per buildvalue/import — with the buildvalue's stage chain
// (buildfrom/pluginresolve/pin/buildrun) collapsed into the row's state.
// Pure functions over api.Snapshot, so every rule is table-testable.

import (
	"sort"
	"strings"

	"github.com/jobs-build/jobs-iroh/api"
	"github.com/jobs-build/jobs-iroh/wire"
)

// stageNames are the display names of the chain stages (+ import).
var stageNames = map[string]string{
	wire.KindBuildFrom:     "eval",
	wire.KindPluginResolve: "resolve",
	wire.KindPin:           "pin",
	wire.KindBuildRun:      "build",
	wire.KindImport:        "fetch",
}

// chainOrder is the buildvalue stage order — folds pick the FIRST match in
// this order so a verdict is deterministic when several stages qualify.
var chainOrder = []string{wire.KindBuildFrom, wire.KindPluginResolve, wire.KindPin, wire.KindBuildRun}

// buildRow is one logical row: a buildvalue (with its chain folded in) or
// an import.
type buildRow struct {
	Node      string // the row's own node name
	Kind      string // wire.KindBuildValue | wire.KindImport
	Label     string
	Phase     string // folded wire phase
	Stage     string // active/failed stage name (eval|resolve|pin|build|fetch)
	ElapsedMs int64  // running: the active stage's; done: the buildrun's
	Cached    bool
	Err       string
	Runner    string
	LogNode   string   // node whose output the pane shows ("" = none)
	Children  []string // child row node names, display order (label, name)
}

// buildGraph is the folded snapshot: logical rows plus the display roots.
type buildGraph struct {
	rows  map[string]*buildRow
	roots []string
}

// SnapshotHasGraph reports whether the snapshot carries dependency edges. A
// single-node closure folds fine without any (the fast-pathed target); a
// multi-node closure without edges is an old server → callers fall back to
// the flat view.
func SnapshotHasGraph(snap api.Snapshot) bool {
	if len(snap.Nodes) <= 1 {
		return true
	}
	for _, n := range snap.Nodes {
		if len(n.Deps) > 0 {
			return true
		}
	}
	return false
}

// foldSnapshot folds the snapshot's raw nodes into the logical row graph.
func foldSnapshot(snap api.Snapshot) *buildGraph {
	byName := make(map[string]api.NodeSnap, len(snap.Nodes))
	for _, n := range snap.Nodes {
		byName[n.Node] = n
	}
	kindOf := func(name string) string {
		kind, _, err := wire.ParseNodeName(name)
		if err != nil {
			return ""
		}
		return kind
	}
	logical := func(kind string) bool {
		return kind == wire.KindBuildValue || kind == wire.KindImport
	}

	g := &buildGraph{rows: map[string]*buildRow{}}
	for _, n := range snap.Nodes {
		kind := kindOf(n.Node)
		if !logical(kind) {
			continue
		}
		row := &buildRow{Node: n.Node, Kind: kind, Label: n.Label}

		// Split deps into the stage chain and direct logical children
		// (imports dep straight on their fetcher buildvalue; buildvalues on
		// their chain — direct logical deps are kept defensively).
		chain := map[string]api.NodeSnap{} // by kind
		var kids []string
		for _, d := range n.Deps {
			dk := kindOf(d)
			switch {
			case logical(dk):
				kids = append(kids, d)
			case dk != "":
				if dn, ok := byName[d]; ok {
					chain[dk] = dn
				}
			}
		}
		// Chain nodes' logical deps are the row's children.
		for _, ck := range chainOrder {
			cn, ok := chain[ck]
			if !ok {
				continue
			}
			for _, d := range cn.Deps {
				if logical(kindOf(d)) {
					kids = append(kids, d)
				}
			}
		}
		row.Children = orderChildren(dedup(kids), byName)

		if kind == wire.KindImport {
			row.Phase, row.Stage = n.Phase, stageNames[wire.KindImport]
			row.ElapsedMs, row.Cached = n.ElapsedMs, n.Cached
			row.Err, row.Runner = n.ErrSummary, n.Runner
			row.LogNode = n.Node
			g.rows[n.Node] = row
			continue
		}
		foldChain(row, n, chain)
		g.rows[n.Node] = row
	}

	g.roots = rootsOf(g)
	return g
}

// foldChain derives a buildvalue row's verdict from its stage chain
// (design §2 precedence: chain failure → own verdict → running → queued →
// done → waiting).
func foldChain(row *buildRow, own api.NodeSnap, chain map[string]api.NodeSnap) {
	br, hasBR := chain[wire.KindBuildRun]

	// The output pane's target, best available: failed stage → active stage
	// → the buildrun (stored logs of the run). Falls out of the fold below
	// for the verdict rows; done/waiting rows default to the buildrun.
	if hasBR {
		row.LogNode = br.Node
	}

	for _, ck := range chainOrder {
		if cn, ok := chain[ck]; ok && cn.Phase == wire.PhaseFailed {
			row.Phase, row.Stage = wire.PhaseFailed, stageNames[ck]
			row.Err, row.Runner, row.ElapsedMs = cn.ErrSummary, cn.Runner, cn.ElapsedMs
			row.LogNode = cn.Node
			return
		}
	}
	switch own.Phase {
	case wire.PhaseFailed, wire.PhaseCancelled, wire.PhaseUpstream:
		row.Phase, row.Err = own.Phase, own.ErrSummary
		return
	}
	for _, ck := range chainOrder {
		if cn, ok := chain[ck]; ok && (cn.Phase == wire.PhaseRunning || cn.Phase == wire.PhasePublishing) {
			row.Phase, row.Stage = cn.Phase, stageNames[ck]
			row.Runner, row.ElapsedMs = cn.Runner, cn.ElapsedMs
			row.LogNode = cn.Node
			return
		}
	}
	for _, ck := range chainOrder {
		if cn, ok := chain[ck]; ok && cn.Phase == wire.PhaseQueued {
			row.Phase, row.Stage = wire.PhaseQueued, stageNames[ck]
			return
		}
	}
	if own.Phase == wire.PhaseDone {
		row.Phase = wire.PhaseDone
		row.Cached = own.Cached || (hasBR && br.Cached)
		if hasBR {
			row.ElapsedMs = br.ElapsedMs
		}
		return
	}
	row.Phase = wire.PhaseWaiting
}

// rootsOf returns the in-degree-0 logical rows (the target; a forest only
// on malformed snapshots), ordered like children. A cyclic malformed graph
// with no in-degree-0 row falls back to the name-sorted first row so the
// tree always renders something.
func rootsOf(g *buildGraph) []string {
	child := map[string]bool{}
	for _, r := range g.rows {
		for _, c := range r.Children {
			child[c] = true
		}
	}
	var roots []string
	for name := range g.rows {
		if !child[name] {
			roots = append(roots, name)
		}
	}
	if len(roots) == 0 && len(g.rows) > 0 {
		for name := range g.rows {
			roots = append(roots, name)
		}
		sort.Strings(roots)
		roots = roots[:1]
	}
	byName := map[string]api.NodeSnap{}
	for name, r := range g.rows {
		byName[name] = api.NodeSnap{Node: name, Label: r.Label}
	}
	return orderChildren(roots, byName)
}

// orderChildren sorts row names for display: by label, then name.
func orderChildren(names []string, byName map[string]api.NodeSnap) []string {
	sort.Slice(names, func(i, j int) bool {
		li, lj := byName[names[i]].Label, byName[names[j]].Label
		if li != lj {
			return li < lj
		}
		return names[i] < names[j]
	})
	return names
}

func dedup(names []string) []string {
	seen := map[string]bool{}
	out := names[:0]
	for _, n := range names {
		if !seen[n] {
			seen[n] = true
			out = append(out, n)
		}
	}
	return out
}

// treeRow is one visible line of the flattened tree.
type treeRow struct {
	Path     string // "/"-joined node names root→row (expansion identity)
	Node     string
	Depth    int
	HasKids  bool
	Expanded bool
}

// maxTreeDepth bounds flatten recursion (cycle/malformed-snapshot guard on
// top of the on-path check).
const maxTreeDepth = 64

// flattenTree renders the DAG as a tree: shared subtrees repeat under every
// parent; expansion is per path. exp overrides the default (expanded,
// except cached-done rows); missing paths use the default.
func flattenTree(g *buildGraph, exp map[string]bool) []treeRow {
	var out []treeRow
	var walk func(name, parentPath string, depth int)
	walk = func(name, parentPath string, depth int) {
		row, ok := g.rows[name]
		if !ok || depth > maxTreeDepth {
			return
		}
		// On-path check: a malformed cycle repeats a name in its own
		// ancestry — skip rather than recurse forever.
		if strings.Contains(parentPath+"/", "/"+name+"/") {
			return
		}
		path := parentPath + "/" + name
		expanded := len(row.Children) > 0
		if expanded {
			if v, ok := exp[path]; ok {
				expanded = v
			} else {
				expanded = !(row.Phase == wire.PhaseDone && row.Cached)
			}
		}
		out = append(out, treeRow{Path: path, Node: name, Depth: depth, HasKids: len(row.Children) > 0, Expanded: expanded})
		if expanded {
			for _, c := range row.Children {
				walk(c, path, depth+1)
			}
		}
	}
	for _, r := range g.roots {
		walk(r, "", 0)
	}
	return out
}
