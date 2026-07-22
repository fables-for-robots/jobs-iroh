//go:build linux

package runner

// The local depth-first build driver — the CLI's "same code paths" orchestrator
// (jobs' develop_linux.go, driver half). The interactive develop PTY shell
// (prepareDevelop/RunDevelop) is a later milestone; this file carries only the
// pieces the local build/run pipeline needs: DevelopConfig, the developDriver
// (memoized, cycle-detected, skip-by-ref-existence ensure loops), and
// outcomeErr.

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/fables-for-robots/amber-store-core/key"
	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/builddef"
	"github.com/fables-for-robots/jobs-iroh/importdef"
)

// DevelopConfig configures a `jobs develop` / local `jobs run --source` run.
type DevelopConfig struct {
	SourceDir string // local dir to ingest as the build source (default ".")
	Dir       string // build root within the source (where BUILD.jobs lives)
	BuildFile string // optional recipe path relative to Dir (default BUILD.jobs)
	Platform  string // default Platform()
	Params    []byte // canonical (CBOR) build params
	ShellRef  string // default "shell:<platform>"
	CacheDir  string // fetcher artifact cache
	Secrets   map[string]TagSecret
}

// developDriver makes a dependency graph present in the local amber store,
// depth-first, reusing the runner's stage drivers. visited de-dups by node.
type developDriver struct {
	ctx        context.Context
	st         *amber.Store
	rw         RefWriter // the local ref publisher for the stage-driver calls (RefWriter seam)
	brc        BuildRunCfg
	secrets    map[string]TagSecret
	visited    map[string]bool
	inProgress map[string]bool
}

// ensureInput makes one builddef.Input's value present: ingest its definition
// (so it is readable by key), then build or import it by kind.
func (d *developDriver) ensureInput(in builddef.Input, p *Progress) error {
	if _, err := d.st.IngestFile(d.ctx, in.Definition); err != nil {
		return fmt.Errorf("ingest input definition: %w", err)
	}
	k, err := in.Key()
	if err != nil {
		return err
	}
	switch in.Kind {
	case builddef.KindImport:
		idef, err := importdef.Decode(in.Definition)
		if err != nil {
			return fmt.Errorf("decode import definition: %w", err)
		}
		return d.ensureImport(k, idef, p)
	case builddef.KindBuild:
		return d.ensureBuild(k, p)
	case builddef.KindTree:
		// A tree input is already-present content (a sub-build's source, a
		// subtree of the build root). There is nothing to build or import; the
		// build-from stage (RunBuildFrom) resolves it directly.
		return nil
	default:
		return fmt.Errorf("unknown input kind %q", in.Kind)
	}
}

func (d *developDriver) ensureImport(k key.Key, idef importdef.Definition, p *Progress) error {
	node := "import|" + k.String()
	if d.visited[node] {
		return nil
	}
	if d.inProgress[node] {
		return fmt.Errorf("dependency cycle detected at %s", node)
	}
	d.inProgress[node] = true
	defer delete(d.inProgress, node)
	label := importFetchLabel(idef)
	if _, ok, err := d.st.GetKey(d.ctx, "import-output:"+k.String()); err != nil {
		return err
	} else if ok {
		p.Cached(label)
		d.visited[node] = true
		return nil
	}
	// A recipe-declared fetcher is an ordinary build dependency: drive its
	// build first (joining the shared content-addressed cache — a fetcher built
	// elsewhere is found, not rebuilt), then RunImport resolves its artifact by
	// content (recipe-declared-fetchers design §9). Named fetchers resolve
	// against the self-seeded refs inside RunImport; a miss fails there with a
	// clear message.
	if len(idef.FetcherDef) > 0 {
		if err := d.ensureInput(builddef.Input{Kind: builddef.KindBuild, Definition: idef.FetcherDef}, p); err != nil {
			return fmt.Errorf("build fetcher for import: %w", err)
		}
	}
	done := p.Start(label)
	out := RunImport(d.ctx, d.st, d.rw, Subprocess{}, d.brc.CacheDir, k, d.secrets, nil)
	if err := outcomeErr("import "+k.String(), out); err != nil {
		done(err)
		return err
	}
	done(nil)
	d.visited[node] = true
	return nil
}

// importFetchLabel renders an import's progress label as "fetch <fetcher> <k=v ...>",
// showing the fetcher name and its arguments (params, sorted by key) instead of
// the opaque content key. Params that aren't a flat JSON object (or are empty)
// render as just "fetch <fetcher>".
func importFetchLabel(idef importdef.Definition) string {
	s := "fetch " + idef.Fetcher
	pj, err := idef.ParamsJSON()
	if err != nil {
		return s
	}
	var m map[string]any
	if json.Unmarshal(pj, &m) != nil || len(m) == 0 {
		return s
	}
	names := make([]string, 0, len(m))
	for name := range m {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		s += fmt.Sprintf(" %s=%v", name, m[name])
	}
	return s
}

func (d *developDriver) ensureBuild(k key.Key, p *Progress) error {
	node := "build|" + k.String()
	if d.visited[node] {
		return nil
	}
	if d.inProgress[node] {
		return fmt.Errorf("dependency cycle detected at %s", node)
	}
	d.inProgress[node] = true
	defer delete(d.inProgress, node)
	// Check via two-hop: build-from:K -> F -> build-output:F.
	if _, ok, err := d.st.ResolveBuildOutput(d.ctx, k); err != nil {
		return err
	} else if ok {
		p.Cached("build " + k.String() + " (build)")
		d.visited[node] = true
		return nil
	}

	// build def (source Input) was ingested by ensureInput; key is k.
	defBytes, err := pullFileBytes(d.ctx, d.st, k)
	if err != nil {
		return fmt.Errorf("read build def %s: %w", k.String(), err)
	}
	def, err := builddef.DecodeDefinition(defBytes)
	if err != nil {
		return fmt.Errorf("decode build def %s: %w", k.String(), err)
	}
	if err := d.ensureInput(def.Source, p.Sub()); err != nil {
		return fmt.Errorf("build %s source: %w", k.String(), err)
	}
	done := p.Start("build-from " + k.String() + " (build)")
	bfOut := RunBuildFrom(d.ctx, d.st, d.rw, d.brc, k)
	if err := outcomeErr("build-from "+k.String(), bfOut); err != nil {
		done(err)
		return err
	}
	done(nil)
	if err := d.driveFStages(bfOut.OutputKey, true, p.Sub()); err != nil {
		return err
	}
	d.visited[node] = true
	return nil
}

// outcomeErr turns a non-success Outcome into a descriptive error.
func outcomeErr(what string, out Outcome) error {
	switch {
	case out.Cancelled:
		return fmt.Errorf("%s cancelled", what)
	case out.Decline:
		return fmt.Errorf("%s declined: %s", what, out.DeclineReason)
	case out.Failed:
		return fmt.Errorf("%s failed (%s, phase %s, exit %d): %s", what, out.Class, out.Phase, out.ExitCode, out.Stderr)
	default:
		return nil
	}
}
