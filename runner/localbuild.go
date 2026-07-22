//go:build linux

package runner

import (
	"context"
	"fmt"

	"github.com/fables-for-robots/amber-store-core/key"
	"github.com/fables-for-robots/jobs-iroh/amber"
	"github.com/fables-for-robots/jobs-iroh/builddef"
)

// localBuildFrom computes the content key F for a LOCAL target build: ingest the
// source dir (honoring .amberignore), take the subtree at cfg.Dir as env/, and
// ingest the build-from tree {env/, params, platform}. The local analogue of
// RunBuildFrom minus the build-from:K→F bridge (a local source has no
// submission K; F is the identity). The build-from tree is shared by key with
// the server's, so a local F equals the server's F for the same environment.
func localBuildFrom(ctx context.Context, st *amber.Store, brc BuildRunCfg, cfg DevelopConfig) (key.Key, error) {
	srcDir := cfg.SourceDir
	if srcDir == "" {
		srcDir = "."
	}
	sourceKey, err := st.IngestSourceDir(ctx, srcDir)
	if err != nil {
		return key.Key{}, fmt.Errorf("ingest source %s: %w", srcDir, err)
	}
	envKey, err := resolveSubtreeKey(ctx, st, sourceKey, cfg.Dir)
	if err != nil {
		return key.Key{}, fmt.Errorf("source dir %q not found: %w", cfg.Dir, err)
	}
	// Same recipe resolution + override normalization as the server's RunBuildFrom,
	// so a local F equals the server's F for the same environment (cache join).
	override, err := resolveRecipeOverride(ctx, st, envKey, cfg.BuildFile, nil)
	if err != nil {
		return key.Key{}, err
	}
	f, err := st.BuildFromTree(ctx, envKey, cfg.Params, brc.Platform, override)
	if err != nil {
		return key.Key{}, fmt.Errorf("assemble build-from tree: %w", err)
	}
	return f, nil
}

// driveFStages drives the F-keyed pipeline for f, skipping any stage whose result
// ref already exists (the join) and reporting each step via p. With runFinal it
// also runs build-output:F (jobs run); without it stops after build-pinned:F
// (jobs develop). It ensures discovered plugins/inputs between stages via the
// developDriver (recursive, nested progress). The drivers write the step refs.
func (d *developDriver) driveFStages(f key.Key, runFinal bool, p *Progress) error {
	// Full join: jobs run can execute an existing build-output:F directly.
	if runFinal {
		if _, ok, err := d.st.GetKey(d.ctx, "build-output:"+f.String()); err != nil {
			return err
		} else if ok {
			p.Cached("build")
			return nil
		}
	}

	pinnedExists, err := refExists(d.ctx, d.st, "build-pinned:"+f.String())
	if err != nil {
		return err
	}
	if pinnedExists {
		p.Cached("plugin-resolve")
		p.Cached("pin")
	} else {
		// plugin-resolve (skip if already resolved)
		prExists, err := refExists(d.ctx, d.st, "build-plugin-resolved:"+f.String())
		if err != nil {
			return err
		}
		if prExists {
			p.Cached("plugin-resolve")
		} else {
			done := p.Start("plugin-resolve")
			if err := outcomeErr("plugin-resolve "+f.String(), RunPluginResolve(d.ctx, d.st, d.rw, d.brc, f)); err != nil {
				done(err)
				return err
			}
			done(nil)
		}
		if err := d.ensurePinDeps(f, p); err != nil {
			return err
		}
		// pin
		done := p.Start("pin")
		if err := outcomeErr("pin "+f.String(), RunPin(d.ctx, d.st, d.rw, d.brc, f)); err != nil {
			done(err)
			return err
		}
		done(nil)
	}

	// ensure the pinned inputs are present (needed for develop's sandbox + run's build)
	if err := d.ensureInputs(f, p); err != nil {
		return err
	}

	if runFinal {
		done := p.Start("build")
		if err := outcomeErr("build "+f.String(), RunBuild(d.ctx, d.st, d.rw, d.brc, NamespaceBuildExecutor{}, f)); err != nil {
			done(err)
			return err
		}
		done(nil)
	}
	return nil
}

// ensurePinDeps reads build-plugin-resolved:F and ensures everything the pin
// stage consumes is built: each plugin AND each resolution dep.
func (d *developDriver) ensurePinDeps(f key.Key, p *Progress) error {
	prKey, ok, err := d.st.GetKey(d.ctx, "build-plugin-resolved:"+f.String())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("build-plugin-resolved:%s missing after resolve", f.String())
	}
	prBytes, err := pullFileBytes(d.ctx, d.st, prKey)
	if err != nil {
		return err
	}
	pr, err := builddef.DecodePluginResolved(prBytes)
	if err != nil {
		return err
	}
	for name, in := range pr.Plugins {
		if err := d.ensureInput(in, p.Sub()); err != nil {
			return fmt.Errorf("plugin %s: %w", name, err)
		}
	}
	// Resolution deps are pin-stage dependencies exactly like plugins
	// (resolution-deps design §6.3): build each before RunPin materializes it.
	for name, in := range pr.Deps {
		if err := d.ensureInput(in, p.Sub()); err != nil {
			return fmt.Errorf("resolution dep %s: %w", name, err)
		}
	}
	return nil
}

// ensureInputs reads build-pinned:F and ensures each build input is built.
func (d *developDriver) ensureInputs(f key.Key, p *Progress) error {
	pinnedKey, ok, err := d.st.GetKey(d.ctx, "build-pinned:"+f.String())
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("build-pinned:%s missing after pin", f.String())
	}
	pinnedBytes, err := pullFileBytes(d.ctx, d.st, pinnedKey)
	if err != nil {
		return err
	}
	pinned, err := builddef.DecodePinned(pinnedBytes)
	if err != nil {
		return err
	}
	for _, pi := range pinned.Inputs {
		if err := d.ensureInput(builddef.Input{Kind: pi.Kind, Definition: pi.Definition}, p.Sub()); err != nil {
			return fmt.Errorf("input %s: %w", pi.Name, err)
		}
	}
	return nil
}

// refExists reports whether a ref name resolves in the store.
func refExists(ctx context.Context, st *amber.Store, name string) (bool, error) {
	_, ok, err := st.GetKey(ctx, name)
	return ok, err
}
