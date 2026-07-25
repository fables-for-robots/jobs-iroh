// Command goplugin is a JOBS build plugin (build.md §6) for Go programs. It reads
// a CBOR request {call:{go_sum:<bytes>}, source} on stdin, turns go.sum into one
// module-fetch import spec per dependency (fetcher "gomod"), and writes the CBOR
// response (an array of {path, version, input}) on stdout. It is network-free and
// statically linked (CGO disabled), so it runs in the hermetic plugin sandbox.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/fxamacker/cbor/v2"
)

// request is the plugin's CBOR stdin payload (mirrors runner.pluginRequest).
// Dir is the consumer package's dir within the mounted source tree
// (sibling-sources design §9); "" for legacy narrow builds.
type request struct {
	Call   map[string]any `cbor:"call"`
	Source string         `cbor:"source"`
	Dir    string         `cbor:"dir"`
}

func main() {
	if err := run(); err != nil {
		// A plugin error is a hard (non-retryable) failure (build.md §6, §11):
		// parsing go.sum has no transient failure mode.
		fmt.Fprintln(os.Stderr, "goplugin:", err)
		os.Exit(1)
	}
}

func run() error {
	in, err := io.ReadAll(os.Stdin)
	if err != nil {
		return fmt.Errorf("read request: %w", err)
	}
	var req request
	if err := cbor.Unmarshal(in, &req); err != nil {
		return fmt.Errorf("decode request: %w", err)
	}

	var goSum []byte
	switch v := req.Call["go_sum"].(type) {
	case []byte:
		goSum = v
	case string:
		goSum = []byte(v)
	default:
		return fmt.Errorf("go_sum kwarg missing or not bytes/string (got %T)", req.Call["go_sum"])
	}

	mods := modulesFromGoSum(goSum)
	out := make([]modOut, 0, len(mods))
	for _, m := range mods {
		spec, err := moduleInput(m)
		if err != nil {
			return fmt.Errorf("module %s@%s: %w", m.Path, m.Version, err)
		}
		out = append(out, modOut{Path: m.Path, Version: m.Version, Input: spec})
	}

	// Monorepo mode (sibling-sources design §12.1): a go_mod (and/or go_work)
	// kwarg switches the response to {modules, sources} — sources are the
	// //-rooted sibling paths of the manifest's relative replace/use targets,
	// for the recipe to forward into build() sources=. Go's main-module-only
	// replace rule makes the consumer's manifest the complete local closure.
	// Without the kwarg the legacy bare-array response is byte-identical.
	var rels []string
	monorepo := false
	for _, kw := range []string{"go_mod", "go_work"} {
		switch v := req.Call[kw].(type) {
		case []byte:
			rels = append(rels, relDirectives(v)...)
			monorepo = true
		case string:
			rels = append(rels, relDirectives([]byte(v))...)
			monorepo = true
		}
	}

	var resp []byte
	if monorepo {
		resp, err = cbor.Marshal(map[string]any{
			"modules": out,
			"sources": siblingSources(rels, req.Dir),
		})
	} else {
		resp, err = cbor.Marshal(out)
	}
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	if _, err := os.Stdout.Write(resp); err != nil {
		return fmt.Errorf("write response: %w", err)
	}
	return nil
}
