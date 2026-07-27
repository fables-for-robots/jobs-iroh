# Source Closure Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** `closure = [...]` on the `build()` return — a COMPLETE cover of the source context (no dir auto-seed) — carried in a new `Pinned.Closure` field, computed for Go by a pure-Go gosha-style transitive import walk in goplugin, allowed for root builds, fenced by a runner ALPN bump; released as v0.12.0.

**Architecture:** Rides the shipped sibling-sources machinery: declaration flows recipe → `BuildResult.Closure` → pin-stage `cover.WalkClosure` (seeds = declared only) → `Pinned.Closure` → `cover.Derive` branch (`PruneTree`) → KP. No `Definition`/K change, no `KPVersion` bump; ALPN `jobs-runner-nats/2.0 → 3.0` fences old runners in both directions.

**Tech Stack:** Go, Starlark (go.starlark.net), fxamacker/cbor, `go/parser` (ImportsOnly), amber-store-core.

**Spec:** `docs/design/2026-07-27-source-closure.md` — read it first. Sibling context: `docs/design/2026-07-26-sibling-sources.md`.

## Global Constraints

- Go toolchain comes from the Nix devShell: run everything as `nix develop -c go test ./<pkg>/...` (or `direnv` env). `GOPRIVATE=github.com/jobs-build/*`.
- **Identity-critical**: do NOT touch chunk params, `NormalizeTree`/`PruneTree` normalization values, or `amber.KPVersion` (stays 2 — spec §7.1).
- `Pinned` without `Closure` must encode **byte-identically** to before (cbor `omitempty`).
- `closure` and `sources` are mutually exclusive at every layer that sees both (recipe eval, `cover.Derive`).
- Every `main()`/sandbox-driving `TestMain` already calls `sandbox.Init()` — don't add new TestMains without it.
- Commit after each task; messages end with `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>`.
- This host runs a production jobs stack — never bind test services to port 5000; use 15000+.

---

### Task 1: `Pinned.Closure` carrier (builddef)

**Files:**
- Modify: `builddef/refs.go:78-88` (Pinned struct)
- Test: `builddef/refs_test.go` (or the existing Pinned encode test file — find with `grep -rln EncodePinned builddef/*_test.go`)

**Interfaces:**
- Produces: `Pinned.Closure []string` (cbor key `"closure"`, omitempty), canonicalized with existing `CanonicalSources`.

- [ ] **Step 1: Write the failing test**

```go
func TestPinnedClosureRoundTripAndByteCompat(t *testing.T) {
	// A Pinned WITHOUT Closure must encode byte-identically whether or not
	// the field exists in the struct (omitempty) — cached pins stay valid.
	base := Pinned{Script: "echo hi", Dir: "svc/api", Sources: []string{"lib/common"}}
	b1, err := EncodePinned(base)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(b1, []byte("closure")) {
		t.Fatalf("closure key leaked into closure-less Pinned: %x", b1)
	}

	withC := Pinned{Script: "echo hi", Dir: "svc/api", Closure: []string{"lib/common", "svc/api/go.mod"}}
	b2, err := EncodePinned(withC)
	if err != nil {
		t.Fatal(err)
	}
	dec, err := DecodePinned(b2)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(dec.Closure, withC.Closure) {
		t.Fatalf("closure round-trip: got %v want %v", dec.Closure, withC.Closure)
	}
}
```

- [ ] **Step 2: Run to verify it fails** — `nix develop -c go test ./builddef/ -run TestPinnedClosureRoundTrip -v` → FAIL (unknown field `Closure`).

- [ ] **Step 3: Implement** — in `builddef/refs.go`, after the `Sources` field:

```go
	// Closure, when non-empty, is a COMPLETE cover of the source context
	// (source-closure design §4): the build dir is NOT auto-seeded and the
	// covered tree is exactly this expanded list. Mutually exclusive with
	// Sources — producers enforce it at eval, cover.Derive re-checks.
	Closure []string `cbor:"closure,omitempty"`
```

- [ ] **Step 4: Run to verify it passes** — same command → PASS. Also `nix develop -c go test ./builddef/`.

- [ ] **Step 5: Commit** — `git add builddef/ && git commit -m "builddef: Pinned.Closure — complete-cover carrier (source-closure §4)"`

---

### Task 2: Recipe surface — `closure=` decode, gates, normalization

**Files:**
- Modify: `recipe/recipe.go` — `BuildResult` struct (~:236-268), `decodeBuildResult` (~:375-491), the eval gate (~:363-370)
- Modify: `recipe/sources.go` — `normalizeBuildSources` (:94-124)
- Test: `recipe/sources_test.go` / wherever existing `sources=` decode tests live (`grep -rln "sources_allow_escaping" recipe/*_test.go`)

**Interfaces:**
- Consumes: nothing new.
- Produces: `BuildResult.Closure []string` (normalized root-relative), eval-time errors:
  - both declared: `"build() declares both closure and sources — closure is a complete cover; drop sources"`
  - sources without widened ctx: existing message (unchanged)
  - generated/allow_escaping without widened ctx AND without closure: `"build() declares generated/sources_allow_escaping but this build has neither a widened context nor a closure"`

- [ ] **Step 1: Write failing tests** (adapt helper names to the existing test file's fixtures — there are existing EvalBuild tests with a fake ContextKey; follow their pattern):

```go
func TestEvalBuildClosure(t *testing.T) {
	// (a) closure decodes + normalizes; allowed at ROOT (zero ContextKey).
	src := []byte(`
def build():
    return struct(inputs={}, env={}, script="s", runtime_deps=[],
                  closure=["//lib/common", "cmd/foo", "//go.mod"])
`)
	res, err := EvalBuild(EvalConfig{Platform: "linux/amd64", Dir: ""}, src, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"lib/common", "cmd/foo", "go.mod"}
	if !reflect.DeepEqual(res.Closure, want) {
		t.Fatalf("closure: got %v want %v", res.Closure, want)
	}

	// (b) closure + sources together → error naming both.
	both := []byte(`
def build():
    return struct(inputs={}, env={}, script="s", runtime_deps=[],
                  closure=["//a"], sources=["//b"])
`)
	if _, err := EvalBuild(EvalConfig{Platform: "linux/amd64", Dir: "x"}, both, nil); err == nil ||
		!strings.Contains(err.Error(), "both closure and sources") {
		t.Fatalf("want both-declared error, got %v", err)
	}

	// (c) generated WITH closure at root is allowed.
	gen := []byte(`
def build():
    return struct(inputs={}, env={}, script="s", runtime_deps=[],
                  closure=["//lib"], generated={"//lib/gen.txt": "x"})
`)
	if _, err := EvalBuild(EvalConfig{Platform: "linux/amd64", Dir: ""}, gen, nil); err != nil {
		t.Fatalf("generated+closure at root: %v", err)
	}

	// (d) generated WITHOUT closure at root still rejected.
	genOnly := []byte(`
def build():
    return struct(inputs={}, env={}, script="s", runtime_deps=[],
                  generated={"//lib/gen.txt": "x"})
`)
	if _, err := EvalBuild(EvalConfig{Platform: "linux/amd64", Dir: ""}, genOnly, nil); err == nil {
		t.Fatal("generated without closure/context: want error")
	}
}
```

- [ ] **Step 2: Run to verify FAIL** — `nix develop -c go test ./recipe/ -run TestEvalBuildClosure -v`

- [ ] **Step 3: Implement**
  1. `BuildResult`: add `Closure []string` next to `Sources` with a doc comment: `// Closure is the COMPLETE declared cover (source-closure design §3); excludes Sources.`
  2. `decodeBuildResult`: alongside the `sources` attr block add:
     ```go
     if cv, cerr := t.Attr("closure"); cerr == nil {
         closureV = cv
     }
     ```
     decode with the existing `decodeSources` (same list-of-strings shape) into `Closure`, and thread through the final `BuildResult{...}` literal.
  3. `normalizeBuildSources`: normalize `r.Closure` element-wise exactly like `r.Sources` (same loop, error prefix `"build() closure[%d]: "`).
  4. Replace the gate at `recipe.go:363-370` with:
     ```go
     widened := cfg.ContextKey != (key.Key{})
     if len(out.Closure) > 0 && len(out.Sources) > 0 {
         return BuildResult{}, fmt.Errorf("build() declares both closure and sources — closure is a complete cover; drop sources")
     }
     if len(out.Sources) > 0 && !widened {
         return BuildResult{}, fmt.Errorf("build() declares sources/generated but this build has no widened context (submit from a repo root, or drop the declarations)")
     }
     if (len(out.Generated) > 0 || len(out.AllowEscaping) > 0) && !widened && len(out.Closure) == 0 {
         return BuildResult{}, fmt.Errorf("build() declares generated/sources_allow_escaping but this build has neither a widened context nor a closure")
     }
     if len(out.Sources) > 0 || len(out.Generated) > 0 || len(out.AllowEscaping) > 0 || len(out.Closure) > 0 {
         if err := normalizeBuildSources(&out, cfg.Dir); err != nil {
             return BuildResult{}, err
         }
     }
     ```

- [ ] **Step 4: Run to verify PASS** — `nix develop -c go test ./recipe/`

- [ ] **Step 5: Commit** — `recipe: closure= complete-cover surface — decode, gates, normalization (source-closure §3)`

---

### Task 3: cover — `WalkClosure` + `Derive` branch + workdir validation

**Files:**
- Modify: `cover/cover.go` — new `WalkClosure` beside `Walk` (:60-91), `Derive` (:259-280)
- Test: `cover/cover_test.go` (existing store/tree fixtures — reuse its helpers)

**Interfaces:**
- Produces:
  - `func WalkClosure(ctx context.Context, st *amber.Store, contextRoot key.Key, dir string, declared, allowEscaping []string) (WalkResult, error)` — seeds = declared only; empty declared ⇒ error; after expansion, when `dir != ""`, at least one covered path must satisfy `p == dir || strings.HasPrefix(p, dir+"/") || strings.HasPrefix(dir, p+"/")` else error `cover: closure does not cover the build dir %q — the sandbox workdir would not exist`.
  - `Derive` honors `p.Closure` first; `Closure`+`Sources` both set ⇒ error `cover: pinned declares both Closure and Sources`.

- [ ] **Step 1: Write failing tests** (reuse the existing test helpers in `cover_test.go` for building a store + ingested fixture tree — read the file first and copy its setup idiom):

```go
func TestWalkClosureSeedsAndWorkdir(t *testing.T) {
	// fixture: lib/common/{a.txt}, services/api/{go.mod}, docs/x.txt
	ctx, st, root := walkFixture(t) // adapt: whatever helper cover_test.go uses to ingest a tree

	// (a) no dir seed: docs and services/api are NOT covered when only lib/common declared with dir="".
	res, err := WalkClosure(ctx, st, root, "", []string{"lib/common"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(res.Paths, []string{"lib/common"}) {
		t.Fatalf("paths: %v", res.Paths)
	}

	// (b) workdir validation: dir covered transitively via its own manifest.
	if _, err := WalkClosure(ctx, st, root, "services/api", []string{"lib/common", "services/api/go.mod"}, nil); err != nil {
		t.Fatalf("covered workdir rejected: %v", err)
	}

	// (c) workdir NOT covered → hard error.
	_, err = WalkClosure(ctx, st, root, "services/api", []string{"lib/common"}, nil)
	if err == nil || !strings.Contains(err.Error(), "does not cover the build dir") {
		t.Fatalf("want workdir error, got %v", err)
	}

	// (d) ancestor cover satisfies the workdir check.
	if _, err := WalkClosure(ctx, st, root, "services/api", []string{"services"}, nil); err != nil {
		t.Fatalf("ancestor cover rejected: %v", err)
	}

	// (e) empty closure → error.
	if _, err := WalkClosure(ctx, st, root, "", nil, nil); err == nil {
		t.Fatal("empty closure accepted")
	}
}

func TestDeriveClosureBranch(t *testing.T) {
	ctx, st, root := walkFixture(t)
	pb := []byte("job-cbor-bytes")
	// Closure prunes exactly like Sources does — the two must agree.
	viaClosure, err := Derive(ctx, st, pb, builddef.Pinned{Closure: []string{"lib/common"}}, "linux/amd64", root)
	if err != nil {
		t.Fatal(err)
	}
	viaSources, err := Derive(ctx, st, pb, builddef.Pinned{Sources: []string{"lib/common"}}, "linux/amd64", root)
	if err != nil {
		t.Fatal(err)
	}
	if viaClosure != viaSources {
		t.Fatalf("closure/sources prune divergence: %s vs %s", viaClosure, viaSources)
	}
	// Both set → error.
	if _, err := Derive(ctx, st, pb, builddef.Pinned{Closure: []string{"a"}, Sources: []string{"b"}}, "linux/amd64", root); err == nil {
		t.Fatal("both Closure and Sources accepted")
	}
}
```

- [ ] **Step 2: Run to verify FAIL** — `nix develop -c go test ./cover/ -run 'TestWalkClosure|TestDeriveClosure' -v`

- [ ] **Step 3: Implement** in `cover/cover.go`:

```go
// WalkClosure expands a COMPLETE declared cover (source-closure design §5):
// unlike Walk, the build dir is NOT auto-seeded — the declared paths are the
// whole cover. Same expansion semantics otherwise (missing declared ⇒ error,
// symlink chase, escape policy). After expansion the closure must cover the
// build dir (a covered path at, under, or above dir) or the pruned tree
// would lack the sandbox workdir — a pin-time error, never a runtime cd
// failure (§5.3 [INV]).
func WalkClosure(ctx context.Context, st *amber.Store, contextRoot key.Key, dir string, declared, allowEscaping []string) (WalkResult, error) {
	if len(declared) == 0 {
		return WalkResult{}, fmt.Errorf("cover: empty closure")
	}
	w := &walker{
		ctx: ctx, st: st, root: contextRoot,
		covered: map[string]bool{},
		visited: map[string]bool{},
		allow:   map[string]bool{},
	}
	for _, p := range allowEscaping {
		w.allow[p] = true
	}
	for _, s := range declared {
		if err := w.cover(s, true); err != nil {
			return WalkResult{}, err
		}
	}
	if dir != "" {
		ok := false
		for p := range w.covered {
			if p == dir || strings.HasPrefix(p, dir+"/") || strings.HasPrefix(dir, p+"/") {
				ok = true
				break
			}
		}
		if !ok {
			return WalkResult{}, fmt.Errorf("cover: closure does not cover the build dir %q — the sandbox workdir would not exist", dir)
		}
	}
	out := make([]string, 0, len(w.covered))
	for p := range w.covered {
		out = append(out, p)
	}
	sort.Strings(out)
	return WalkResult{Paths: out, Warnings: w.warnings}, nil
}
```

`Derive`: replace the two-way branch with (and extend the doc comment: closure branch first, mutual exclusion re-checked):

```go
	if len(p.Closure) > 0 && len(p.Sources) > 0 {
		return key.Key{}, fmt.Errorf("cover: pinned declares both Closure and Sources")
	}
	var covered key.Key
	var err error
	switch {
	case len(p.Closure) > 0:
		covered, err = st.PruneTree(ctx, contextRoot, p.Closure)
	case len(p.Sources) > 0:
		covered, err = st.PruneTree(ctx, contextRoot, p.Sources)
	default:
		covered, err = st.NormalizeTree(ctx, contextRoot)
	}
```

Add `"strings"` to imports if absent.

- [ ] **Step 4: Run to verify PASS** — `nix develop -c go test ./cover/`

- [ ] **Step 5: Commit** — `cover: WalkClosure (no dir seed, workdir [INV]) + Derive closure branch (source-closure §5-§6)`

---

### Task 4: Pin-stage wiring (runner/buildeval.go)

**Files:**
- Modify: `runner/buildeval.go:178-235` (walk invocation + `Pinned` literal)
- Test: covered by Task 6 e2e; this task's gate is `go build ./...` + existing suites staying green.

**Interfaces:**
- Consumes: `cover.WalkClosure` (Task 3), `BuildResult.Closure` (Task 2), `Pinned.Closure` (Task 1).
- Produces: pinned blobs whose `Closure` is the expanded complete cover; `Sources` stays nil when closure declared. Root builds (zero `ContextKey`) walk against `env.SourceContentKey`.

- [ ] **Step 1: Implement** — replace the covered-closure block (`var covered []string ... else if len(res.Sources) > 0 {...}`) with:

```go
	var covered, closure []string
	switch {
	case len(res.Closure) > 0:
		// Complete cover (source-closure design §5): dir is not auto-seeded;
		// root builds (no widened context) walk the build-root tree — the
		// same tree Derive later receives as the F env/.
		root := env.ContextKey
		if root == (key.Key{}) {
			root = env.SourceContentKey
		}
		walk, werr := cover.WalkClosure(ctx, st, root, env.Dir, res.Closure, res.AllowEscaping)
		if werr != nil {
			return hard("covering", werr.Error(), 0)
		}
		if len(walk.Warnings) > 0 && pluginErrSink != nil {
			for _, w := range walk.Warnings {
				fmt.Fprintf(pluginErrSink, "cover: %s: %s\n", w.Path, w.Msg)
			}
		}
		closure = walk.Paths
	case env.ContextKey != (key.Key{}):
		walk, werr := cover.Walk(ctx, st, env.ContextKey, env.Dir, res.Sources, res.AllowEscaping)
		if werr != nil {
			return hard("covering", werr.Error(), 0)
		}
		if len(walk.Warnings) > 0 && pluginErrSink != nil {
			for _, w := range walk.Warnings {
				fmt.Fprintf(pluginErrSink, "cover: %s: %s\n", w.Path, w.Msg)
			}
		}
		covered = walk.Paths
	case len(res.Sources) > 0:
		return hard("covering", "recipe declares sources= but the build has no widened context", 0)
	}
```

and in the `builddef.Pinned` literal add `Closure: builddef.CanonicalSources(closure),` next to `Sources:`.

- [ ] **Step 2: Verify** — `nix develop -c go build ./... && nix develop -c go test ./runner/ ./recipe/ ./cover/` (existing suites must stay green; closure e2e lands in Task 6).

- [ ] **Step 3: Commit** — `runner: pin-stage closure wiring — WalkClosure for complete covers, root builds included (source-closure §5)`

---

### Task 5: goplugin — `go_closure` pure-Go transitive import walk

**Files:**
- Create: `plugins/goplugin/closure.go`
- Modify: `plugins/goplugin/main.go` (kwarg + response key)
- Test: `plugins/goplugin/closure_test.go`

**Interfaces:**
- Consumes: `relDirectives` (`gomod.go:17`), `request{Call, Source, Dir}` (`main.go:19-23`).
- Produces: `func goClosure(srcRoot, dir string, gomod, gowork []byte, entries []string) ([]string, error)` returning sorted `//`-rooted covered paths; response map key `"closure"` in monorepo mode. `go_closure` without `go_mod` ⇒ error.

- [ ] **Step 1: Write failing tests**

```go
func TestGoClosure(t *testing.T) {
	root := t.TempDir()
	write := func(p, content string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// Consumer module svc/api → imports sibling lib/common (pkg + tagged pkg
	// + test-only pkg), NOT lib/common/unused. External + stdlib skipped.
	write("svc/api/go.mod", "module example.com/api\n\nreplace example.com/common => ../../lib/common\n")
	write("svc/api/go.sum", "example.com/ext v1.0.0 h1:x\n")
	write("svc/api/main.go", `package main

import (
	"fmt"

	"example.com/common/core"
	"example.com/ext/pkg"
)

func main() { fmt.Println(core.V, pkg.V) }
`)
	// Build-tag-ignored file still contributes imports (cross-compile safety).
	write("svc/api/other_windows.go", "//go:build windows\n\npackage main\n\nimport _ \"example.com/common/winonly\"\n")
	// _test.go imports contribute too.
	write("svc/api/main_test.go", "package main\n\nimport _ \"example.com/common/testutil\"\n")
	write("lib/common/go.mod", "module example.com/common\n")
	write("lib/common/go.sum", "")
	write("lib/common/core/core.go", "package core\n\nimport _ \"example.com/common/deep\"\n\nvar V = 1\n")
	write("lib/common/deep/deep.go", "package deep\n")
	write("lib/common/winonly/w.go", "package winonly\n")
	write("lib/common/testutil/t.go", "package testutil\n")
	write("lib/common/unused/u.go", "package unused\n")

	gomod, _ := os.ReadFile(filepath.Join(root, "svc/api/go.mod"))
	got, err := goClosure(root, "svc/api", gomod, nil, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		"//lib/common/core",
		"//lib/common/deep",
		"//lib/common/go.mod",
		"//lib/common/go.sum",
		"//lib/common/testutil",
		"//lib/common/winonly",
		"//svc/api", // entry dir "." resolves to the consumer package dir
		"//svc/api/go.mod",
		"//svc/api/go.sum",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("closure:\n got %v\nwant %v", got, want)
	}
	// lib/common/unused must NOT be covered.
	for _, p := range got {
		if strings.Contains(p, "unused") {
			t.Fatalf("unused package covered: %v", got)
		}
	}
}

func TestGoClosureNestedCollapse(t *testing.T) {
	root := t.TempDir()
	write := func(p, content string) {
		t.Helper()
		full := filepath.Join(root, filepath.FromSlash(p))
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	// Entry dir covers the module root package; nested reached package
	// collapses under it.
	write("go.mod", "module example.com/m\n")
	write("main.go", "package main\n\nimport _ \"example.com/m/sub\"\n")
	write("sub/s.go", "package sub\n")
	gomod, _ := os.ReadFile(filepath.Join(root, "go.mod"))
	got, err := goClosure(root, "", gomod, nil, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	// "." (module root) cannot be a covered path (NormalizeSourcePath rejects
	// root) — the root package's FILES are enumerated instead; sub is covered
	// as a dir. go.mod/go.sum ride as files.
	for _, p := range got {
		if p == "//." || p == "//" {
			t.Fatalf("root covered wholesale: %v", got)
		}
	}
	wantMembers := []string{"//go.mod", "//main.go", "//sub"}
	for _, w := range wantMembers {
		if !slices.Contains(got, w) {
			t.Fatalf("missing %s in %v", w, got)
		}
	}
}

func TestGoClosureErrors(t *testing.T) {
	root := t.TempDir()
	os.MkdirAll(filepath.Join(root, "app"), 0o755)
	os.WriteFile(filepath.Join(root, "app/go.mod"), []byte("module example.com/app\n\nreplace example.com/gone => ../gone\n"), 0o644)
	os.WriteFile(filepath.Join(root, "app/main.go"), []byte("package main\n\nimport _ \"example.com/gone/x\"\n"), 0o644)
	gomod, _ := os.ReadFile(filepath.Join(root, "app/go.mod"))
	// replace target without go.mod → hard error naming the target.
	if _, err := goClosure(root, "app", gomod, nil, []string{"."}); err == nil {
		t.Fatal("missing sibling go.mod accepted")
	}
	// entry dir with no .go files → hard error.
	os.MkdirAll(filepath.Join(root, "empty"), 0o755)
	if _, err := goClosure(root, "app", []byte("module example.com/app\n"), nil, []string{"../empty"}); err == nil {
		t.Fatal("empty entry dir accepted")
	}
}
```

- [ ] **Step 2: Run to verify FAIL** — `nix develop -c go test ./plugins/goplugin/ -run TestGoClosure -v`

- [ ] **Step 3: Implement `plugins/goplugin/closure.go`** — the shape (adjust as the tests demand, keep it deterministic: sorted dir listings, sorted output):

```go
package main

import (
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
)

// goClosure computes the transitive local-package closure of the entry
// package dirs (source-closure design §8): pure filesystem + go/parser, no
// toolchain, deterministic. Covered output is //-rooted: reached package
// dirs (recursive covers — embeds/cgo/asm/testdata ride along), each
// involved module's go.mod/go.sum, and go.work(.sum) at the context root
// when present. Imports from EVERY .go file count — build-tag-ignored and
// _test.go included (cross-compile and `go test` safety; gosha's
// IgnoredFiles rationale). Stdlib (first path element without a dot) and
// external modules (the go.sum fetcher's territory) are skipped.
func goClosure(srcRoot, dir string, gomod, gowork []byte, entries []string) ([]string, error) {
	consumerPath := modulePath(gomod)
	if consumerPath == "" {
		return nil, fmt.Errorf("go_closure: go_mod has no module directive")
	}
	// Local module map: module path → root-relative dir.
	mods := map[string]string{consumerPath: dir}
	rels := relDirectives(gomod)
	if len(gowork) > 0 {
		rels = append(rels, relDirectives(gowork)...)
	}
	for _, r := range rels {
		mdir := path.Clean(path.Join(dir, r))
		if mdir == "." {
			mdir = ""
		}
		if strings.HasPrefix(mdir, "../") || mdir == ".." {
			return nil, fmt.Errorf("go_closure: replace/use target %q escapes the context root", r)
		}
		mb, err := os.ReadFile(filepath.Join(srcRoot, filepath.FromSlash(mdir), "go.mod"))
		if err != nil {
			return nil, fmt.Errorf("go_closure: sibling %q has no readable go.mod: %w", mdir, err)
		}
		mp := modulePath(mb)
		if mp == "" {
			return nil, fmt.Errorf("go_closure: sibling %q go.mod has no module directive", mdir)
		}
		mods[mp] = mdir
	}

	// Transitive walk from the entry package dirs.
	frontier := make([]string, 0, len(entries))
	for _, e := range entries {
		ed := path.Clean(path.Join(dir, e))
		if ed == "." {
			ed = ""
		}
		if strings.HasPrefix(ed, "../") || ed == ".." {
			return nil, fmt.Errorf("go_closure: entry %q escapes the context root", e)
		}
		frontier = append(frontier, ed)
	}
	visited := map[string]bool{}
	usedMods := map[string]bool{consumerPath: true}
	for len(frontier) > 0 {
		pkgDir := frontier[0]
		frontier = frontier[1:]
		if visited[pkgDir] {
			continue
		}
		visited[pkgDir] = true
		imports, n, err := dirImports(filepath.Join(srcRoot, filepath.FromSlash(pkgDir)))
		if err != nil {
			return nil, fmt.Errorf("go_closure: %s: %w", pkgDir, err)
		}
		if n == 0 {
			return nil, fmt.Errorf("go_closure: %q contains no .go files", pkgDir)
		}
		for _, imp := range imports {
			mp, mdir, ok := resolveLocal(mods, imp)
			if !ok {
				continue // stdlib or external
			}
			pd := path.Join(mdir, strings.TrimPrefix(strings.TrimPrefix(imp, mp), "/"))
			if pd == "." {
				pd = ""
			}
			usedMods[mp] = true
			if !visited[pd] {
				frontier = append(frontier, pd)
			}
		}
	}

	// Assemble output: package dirs (module-root packages enumerate their
	// FILES — the root of a cover cannot be "." and a whole-module cover
	// would defeat precision) + involved module manifests + go.work files.
	out := map[string]bool{}
	for pd := range visited {
		if pd == "" || mods[moduleOf(mods, pd)] == pd && isModuleRoot(mods, pd) {
			// module-root package: enumerate its files (non-recursive).
			files, err := rootPackageFiles(srcRoot, pd)
			if err != nil {
				return nil, err
			}
			for _, f := range files {
				out[path.Join(pd, f)] = true
			}
			continue
		}
		out[pd] = true
	}
	for mp := range usedMods {
		mdir := mods[mp]
		out[path.Join(mdir, "go.mod")] = true
		if _, err := os.Stat(filepath.Join(srcRoot, filepath.FromSlash(mdir), "go.sum")); err == nil {
			out[path.Join(mdir, "go.sum")] = true
		}
	}
	if len(gowork) > 0 {
		for _, f := range []string{"go.work", "go.work.sum"} {
			if _, err := os.Stat(filepath.Join(srcRoot, f)); err == nil {
				out[f] = true
			}
		}
	}

	// Collapse nested under covered ancestors, sort, //-root.
	paths := make([]string, 0, len(out))
	for p := range out {
		paths = append(paths, p)
	}
	sort.Strings(paths)
	collapsed := make([]string, 0, len(paths))
	for _, p := range paths {
		if n := len(collapsed); n > 0 && strings.HasPrefix(p, collapsed[n-1]+"/") {
			continue
		}
		collapsed = append(collapsed, p)
	}
	final := make([]string, len(collapsed))
	for i, p := range collapsed {
		final[i] = "//" + p
	}
	return final, nil
}
```

plus the small helpers (each a handful of lines — write them in the same file):

```go
// modulePath extracts the module directive's path from go.mod bytes.
func modulePath(gomod []byte) string {
	for _, line := range strings.Split(string(gomod), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "module ") {
			return strings.Trim(strings.TrimSpace(strings.TrimPrefix(line, "module ")), `"`)
		}
	}
	return ""
}

// dirImports parses EVERY .go file in one directory (ImportsOnly) and
// returns the union of import paths, plus the .go file count.
func dirImports(hostDir string) ([]string, int, error) {
	ents, err := os.ReadDir(hostDir)
	if err != nil {
		return nil, 0, err
	}
	fset := token.NewFileSet()
	seen := map[string]bool{}
	n := 0
	for _, e := range ents {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") {
			continue
		}
		n++
		f, err := parser.ParseFile(fset, filepath.Join(hostDir, e.Name()), nil, parser.ImportsOnly)
		if err != nil {
			return nil, 0, fmt.Errorf("%s: %w", e.Name(), err)
		}
		for _, im := range f.Imports {
			seen[strings.Trim(im.Path.Value, `"`)] = true
		}
	}
	out := make([]string, 0, len(seen))
	for p := range seen {
		out = append(out, p)
	}
	sort.Strings(out)
	return out, n, nil
}

// resolveLocal longest-prefix-matches imp against the local module map.
// Stdlib (first element without a dot) and unmatched (external) → ok=false.
func resolveLocal(mods map[string]string, imp string) (mp, dir string, ok bool) {
	first := imp
	if i := strings.IndexByte(imp, '/'); i >= 0 {
		first = imp[:i]
	}
	if !strings.Contains(first, ".") {
		return "", "", false // stdlib (incl. the cgo pseudo-import "C")
	}
	best := ""
	for m := range mods {
		if (imp == m || strings.HasPrefix(imp, m+"/")) && len(m) > len(best) {
			best = m
		}
	}
	if best == "" {
		return "", "", false // external — the go.sum fetcher's territory
	}
	return best, mods[best], true
}
```

**Note on the module-root-package edge:** `NormalizeSourcePath` rejects paths resolving to the context root, and covering a whole module root recursively would defeat the precision the feature exists for. When a *reached package dir* is exactly a module root (consumer `dir` itself, a sibling root, or `""` for a root build), enumerate that directory's regular files non-recursively (`rootPackageFiles`: `os.ReadDir`, keep files only, skip `.git`; return sorted names) instead of covering the dir. Write `moduleOf`/`isModuleRoot` as trivial helpers over the `mods` map (longest dir-prefix match; equality check). Simplify while implementing if a cleaner arrangement passes the same tests — the tests are the contract, including `TestGoClosureNestedCollapse` asserting the root is never covered wholesale.

**In `main.go`**, after the monorepo block:

```go
	var closure []string
	if cv, ok := req.Call["go_closure"]; ok {
		entries, err := stringList(cv) // helper: []any|[]string → []string, error otherwise
		if err != nil {
			return fmt.Errorf("go_closure kwarg: %w", err)
		}
		var gm, gw []byte
		// reuse the bytes already extracted for the monorepo block (restructure
		// that loop so go_mod/go_work land in named vars gm, gw)
		if len(gm) == 0 {
			return fmt.Errorf("go_closure requires go_mod")
		}
		closure, err = goClosure(req.Source, req.Dir, gm, gw, entries)
		if err != nil {
			return err
		}
	}
```

and include `"closure": closure` in the monorepo-mode response map only when non-nil. The legacy bare-array response stays byte-identical when neither kwarg is present.

- [ ] **Step 4: Run to verify PASS** — `nix develop -c go test ./plugins/goplugin/ -v`

- [ ] **Step 5: Commit** — `goplugin: go_closure — pure-Go transitive import walk at package-dir granularity (source-closure §8)`

---

### Task 6: e2e + sched crash-window tests

**Files:**
- Modify: `runner/monorepo_linux_test.go` (add closure e2e) or create `runner/closure_linux_test.go` in the same idiom
- Modify: the sched KP crash-window test file (`grep -rln resolveKP sched/*_test.go`)

**Interfaces:** consumes everything above; no new API.

- [ ] **Step 1: Write the widened-closure e2e** (new test in `runner`, reusing `devSetup`, `writeMonorepo` patterns — read `monorepo_linux_test.go` first). Recipe under test:

```python
def build():
    return struct(
        inputs = {},
        env = {},
        script = '''
if [ -e "$SRC_ROOT/docs" ]; then echo "uncovered docs/ leaked" >&2; exit 1; fi
if [ -e "$SRC_ROOT/services/api/notes.md" ]; then echo "uncovered notes.md leaked" >&2; exit 1; fi
cat "$SRC_ROOT/lib/common/common.txt" > "$out/result"
cat go.mod > "$out/gomod"
''',
        runtime_deps = [],
        closure = ["//lib/common", "//services/api/go.mod", "//services/api/BUILD.jobs"],
    )
```

Fixture adds `services/api/go.mod` (`module example.com/api\n`) and `services/api/notes.md`. Assertions, in the existing `build()`/`readOut`/`cachedLine` idiom:
1. First build succeeds; `$SRC` contains only the closure (script asserts).
2. Edit `services/api/notes.md` (INSIDE the build dir but OUTSIDE the closure) → rebuild pipeline reruns pin (F changes) but the buildrun is a **cache hit** (`✓ build  (cached)` present, same KP) — this is the consumer-dir narrowing win.
3. Edit `lib/common/common.txt` → NOT cached, new KP, new output.
4. `monoTouchAll` mtime churn → cache hit (KP mtime-immunity holds for closure builds).

- [ ] **Step 2: Write the root-build closure e2e** — same file: fixture with `go.mod`, `BUILD.jobs`, `cmd/foo/foo.txt`, `cmd/bar/bar.txt` at the ROOT (`Dir: ""` in `DevelopConfig`), recipe:

```python
def build():
    return struct(
        inputs = {},
        env = {},
        script = '''
if [ -e "$SRC_ROOT/cmd/bar" ]; then echo "uncovered cmd/bar leaked" >&2; exit 1; fi
cat cmd/foo/foo.txt > "$out/result"
''',
        runtime_deps = [],
        closure = ["//cmd/foo", "//go.mod", "//BUILD.jobs"],
    )
```

Assert: builds; edit `cmd/bar/bar.txt` → buildrun cache hit; edit `cmd/foo/foo.txt` → rebuild.

- [ ] **Step 3: Run** — `nix develop -c go test ./runner/ -run 'Closure' -v` (sandbox tests need Linux namespaces — they run like the existing monorepo test). Expected: PASS.

- [ ] **Step 4: sched crash-window test** — in the existing KP test file, add a case: write a `build-pinned:F` whose `Pinned` carries `Closure` (reuse the file's existing store fixture; encode via `builddef.EncodePinned`), delete/skip the `pin-cover` ref, call the code path that hits `resolveKPLocked`/`deriveKP` the way the existing cases do, and assert the derived KP equals a direct `cover.Derive` call with the same inputs. Follow the surrounding tests' setup verbatim — this asserts the server-side derivation honors `Closure` and that re-derivation after the crash window converges.

- [ ] **Step 5: Run** — `nix develop -c go test ./sched/ -run KP -v` → PASS, then the full suite: `nix develop -c go test ./...`

- [ ] **Step 6: Commit** — `runner+sched: closure e2e (consumer-dir narrowing, root builds, mtime immunity) + KP crash-window coverage (source-closure §11)`

---

### Task 7: ALPN fence + docs

**Files:**
- Modify: `serve/serve.go:47`, `runnerd/runnerd.go:54`, `amberclient/conn.go:17` (comment only)
- Modify: `CLAUDE.md` (ALPN list in the intro ¶; sibling-sources invariant block; `cover/` + `recipe/` package-map rows)
- Modify: `docs/architecture/architecture.md` — only if quick: add a one-line note in the ref-namespace section that `Pinned.Closure` complete covers exist (spec §12 allows deferring the stale-table rewrite).

- [ ] **Step 1: Bump the ALPN** — in BOTH `serve/serve.go` and `runnerd/runnerd.go`: `"jobs-runner-nats/2.0"` → `"jobs-runner-nats/3.0"`. Extend the comment in `serve.go` with one sentence: `Bumped to 3.0 for source closures: an old runner's recipe decoder silently ignores closure= and would fork pin-cover/<v>:F content (source-closure design §7.2).` Update the `amberclient/conn.go` docstring mention to `/3.0`.

- [ ] **Step 2: Grep for stragglers** — `grep -rn "jobs-runner-nats" --include='*.go' --include='*.md' | grep -v docs/design | grep -v docs/research` — every remaining non-historical mention must say 3.0 (CLAUDE.md intro currently says 1.0 — fix to 3.0).

- [ ] **Step 3: CLAUDE.md invariant block** — in the **Sibling sources** bullet, append: closure semantics (`closure=` is a COMPLETE cover — no dir seed, mutually exclusive with `sources`, workdir coverage validated at pin), root builds may declare it, and the ALPN is now `jobs-runner-nats/3.0`. In the package map: `cover/` row gains "WalkClosure (complete covers, no dir seed)"; `recipe/` (builddef row) unchanged; add `go_closure` to the `fetchers/, plugins/` row.

- [ ] **Step 4: Verify** — `nix develop -c go test ./serve/ ./runnerd/ ./amberclient/` and `nix develop -c go build ./...`.

- [ ] **Step 5: Commit** — `serve+runnerd: jobs-runner-nats/3.0 — fence pre-closure runners (source-closure §7.2); docs`

---

### Task 8: Full verification + release v0.12.0

**Files:**
- Modify: `version/version.go` (`0.11.0` → `0.12.0`)
- Read first: `deploy/jobs-registry/` (Dockerfile + any script/README) for the exact docker publish incantation used for v0.11.0.

- [ ] **Step 1: Full clean test run** — `nix develop -c go build ./... && nix develop -c go test ./...` → ALL PASS. Fix anything red before proceeding (superpowers:verification-before-completion — paste the actual output summary into the commit/PR notes, no success claims without it).

- [ ] **Step 2: Version bump commit** —

```bash
# in version/version.go: const Version = "0.12.0"
git add version/version.go
git commit -m "Release v0.12.0: source closure — precise build-input enumeration"
```

- [ ] **Step 3: Tag + push + GitHub release** —

```bash
git tag v0.12.0
git push origin main --tags
nix develop -c gh release create v0.12.0 --title "v0.12.0: source closure" \
  --notes "closure= on build(): complete source-context cover (no dir seed), Pinned.Closure carrier, root-build support, goplugin go_closure pure-Go transitive import walk (embed/cgo/asm safe at package-dir granularity). Runner ALPN fence bumped to jobs-runner-nats/3.0 — 0.12 runners require a 0.12 server and vice versa. Design: docs/design/2026-07-27-source-closure.md"
```

- [ ] **Step 4: Docker publish** (per the established process — sudo docker, buildx builder `jobs-multi`, multi-arch):

```bash
# Verify the exact pattern against deploy/jobs-registry first; expected shape:
sudo docker buildx build --builder jobs-multi \
  --platform linux/amd64,linux/arm64 \
  -f deploy/jobs-registry/Dockerfile \
  -t dmilhdef/jobs-registry:v0.12.0 -t dmilhdef/jobs-registry:latest \
  --push .
```

- [ ] **Step 5: Post-release sanity** — `gh release view v0.12.0` shows the release; `sudo docker manifest inspect dmilhdef/jobs-registry:v0.12.0 | head` shows both architectures.

---

## Self-review (done at plan-writing time)

- **Spec coverage:** §3 surface → Task 2; §4 carrier → Task 1; §5 walk/root/workdir → Tasks 3-4; §6 derive → Task 3; §7 fencing → Task 7; §8 plugin → Task 5; §9 errors → Tasks 2/3/5 error cases; §11 tests → Tasks 1-6; §12 docs → Task 7; release → Task 8.
- **Known judgment point:** the module-root-package file-enumeration edge in Task 5 (root cover is rejected by `NormalizeSourcePath` by design). The tests pin the observable contract; the helper arrangement is the implementer's choice.
- **Type consistency:** `WalkClosure` signature (Task 3) matches its Task 4 call site; `BuildResult.Closure`/`Pinned.Closure` names consistent across Tasks 1, 2, 4; `goClosure` signature matches its `main.go` call.
