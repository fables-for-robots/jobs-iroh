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
	gm, gmOK := kwargBytes(req.Call["go_mod"])
	gw, gwOK := kwargBytes(req.Call["go_work"])
	monorepo := gmOK || gwOK
	if gmOK {
		rels = append(rels, relDirectives(gm)...)
	}
	if gwOK {
		rels = append(rels, relDirectives(gw)...)
	}

	// Closure mode (source-closure design §8): go_closure lists the entry
	// package dirs (dir-relative); the response gains a "closure" key — the
	// //-rooted complete cover for the recipe to forward into build()
	// closure=. Requires go_mod (the consumer's module path anchors import
	// resolution).
	var closure []string
	if cv, ok := req.Call["go_closure"]; ok {
		entries, err := stringList(cv)
		if err != nil {
			return fmt.Errorf("go_closure kwarg: %w", err)
		}
		if !gmOK {
			return fmt.Errorf("go_closure requires go_mod")
		}
		closure, err = goClosure(req.Source, req.Dir, gm, gw, entries)
		if err != nil {
			return err
		}
	}

	var resp []byte
	if monorepo {
		m := map[string]any{
			"modules": out,
			"sources": siblingSources(rels, req.Dir),
		}
		if closure != nil {
			m["closure"] = closure
		}
		resp, err = cbor.Marshal(m)
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

// kwargBytes coerces a CBOR kwarg value to bytes ([]byte or string shapes).
func kwargBytes(v any) ([]byte, bool) {
	switch b := v.(type) {
	case []byte:
		return b, true
	case string:
		return []byte(b), true
	}
	return nil, false
}

// stringList coerces a CBOR kwarg value to a list of strings.
func stringList(v any) ([]string, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("must be a list of strings (got %T)", v)
	}
	out := make([]string, 0, len(l))
	for i, e := range l {
		switch s := e.(type) {
		case string:
			out = append(out, s)
		case []byte:
			out = append(out, string(s))
		default:
			return nil, fmt.Errorf("element %d is not a string (got %T)", i, e)
		}
	}
	return out, nil
}
