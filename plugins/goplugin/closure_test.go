package main

import (
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func writeTree(t *testing.T, root string, files map[string]string) {
	t.Helper()
	for p, content := range files {
		full := filepath.Join(root, filepath.FromSlash(p))
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestGoClosure(t *testing.T) {
	root := t.TempDir()
	// Consumer module svc/api imports sibling lib/common packages (regular +
	// build-tag-ignored + test-only), NOT lib/common/unused. External and
	// stdlib imports are skipped.
	writeTree(t, root, map[string]string{
		"svc/api/go.mod": "module example.com/api\n\nreplace example.com/common => ../../lib/common\n",
		"svc/api/go.sum": "example.com/ext v1.0.0 h1:x\n",
		"svc/api/main.go": `package main

import (
	"fmt"

	"example.com/common/core"
	"example.com/ext/pkg"
)

func main() { fmt.Println(core.V, pkg.V) }
`,
		// Build-tag-ignored file still contributes imports (cross-compile safety).
		"svc/api/other_windows.go": "//go:build windows\n\npackage main\n\nimport _ \"example.com/common/winonly\"\n",
		// _test.go imports contribute too (`go test` safety).
		"svc/api/main_test.go":     "package main\n\nimport _ \"example.com/common/testutil\"\n",
		"lib/common/go.mod":        "module example.com/common\n",
		"lib/common/go.sum":        "",
		"lib/common/core/core.go":  "package core\n\nimport _ \"example.com/common/deep\"\n\nvar V = 1\n",
		"lib/common/deep/deep.go":  "package deep\n",
		"lib/common/winonly/w.go":  "package winonly\n",
		"lib/common/testutil/t.go": "package testutil\n",
		"lib/common/unused/u.go":   "package unused\n",
	})

	gomod, err := os.ReadFile(filepath.Join(root, "svc/api/go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := goClosure(root, "svc/api", gomod, nil, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	// svc/api is the consumer MODULE ROOT package: its files are enumerated
	// non-recursively (covering the dir would cover the whole module).
	want := []string{
		"//lib/common/core",
		"//lib/common/deep",
		"//lib/common/go.mod",
		"//lib/common/go.sum",
		"//lib/common/testutil",
		"//lib/common/winonly",
		"//svc/api/go.mod",
		"//svc/api/go.sum",
		"//svc/api/main.go",
		"//svc/api/main_test.go",
		"//svc/api/other_windows.go",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("closure:\n got %v\nwant %v", got, want)
	}
	for _, p := range got {
		if strings.Contains(p, "unused") {
			t.Fatalf("unused package covered: %v", got)
		}
	}
}

func TestGoClosureRootModuleAndEmbed(t *testing.T) {
	root := t.TempDir()
	// Root build (dir ""): module root is itself a package with an embed of a
	// subdir — the embed target must be covered even though root-package
	// files are enumerated non-recursively.
	writeTree(t, root, map[string]string{
		"go.mod": "module example.com/m\n",
		"main.go": `package main

import (
	"embed"

	_ "example.com/m/sub"
)

//go:embed static/*
var f embed.FS
`,
		"sub/s.go":       "package sub\n",
		"static/tpl.txt": "tpl\n",
		"docs/notes.md":  "uncovered\n",
	})
	gomod, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := goClosure(root, "", gomod, nil, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range got {
		if p == "//." || p == "//" {
			t.Fatalf("root covered wholesale: %v", got)
		}
		if strings.Contains(p, "docs") {
			t.Fatalf("unrelated docs covered: %v", got)
		}
	}
	for _, w := range []string{"//go.mod", "//main.go", "//sub", "//static/tpl.txt"} {
		if !slices.Contains(got, w) {
			t.Fatalf("missing %s in %v", w, got)
		}
	}
}

func TestGoClosureNestedCollapse(t *testing.T) {
	root := t.TempDir()
	// A reached package nested under another reached package collapses.
	writeTree(t, root, map[string]string{
		"app/go.mod":         "module example.com/app\n",
		"app/main.go":        "package main\n\nimport (\n\t_ \"example.com/app/pkg\"\n\t_ \"example.com/app/pkg/inner\"\n)\n",
		"app/pkg/p.go":       "package pkg\n",
		"app/pkg/inner/i.go": "package inner\n",
	})
	gomod, err := os.ReadFile(filepath.Join(root, "app/go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := goClosure(root, "app", gomod, nil, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	if slices.Contains(got, "//app/pkg/inner") {
		t.Fatalf("nested package not collapsed under //app/pkg: %v", got)
	}
	if !slices.Contains(got, "//app/pkg") {
		t.Fatalf("missing //app/pkg: %v", got)
	}
}

func TestGoClosureErrors(t *testing.T) {
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app/go.mod":  "module example.com/app\n\nreplace example.com/gone => ../gone\n",
		"app/main.go": "package main\n\nimport _ \"example.com/gone/x\"\n",
	})
	gomod, err := os.ReadFile(filepath.Join(root, "app/go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	// replace target without go.mod → hard error naming the target.
	if _, err := goClosure(root, "app", gomod, nil, []string{"."}); err == nil ||
		!strings.Contains(err.Error(), "gone") {
		t.Fatalf("missing sibling go.mod: want error naming it, got %v", err)
	}
	// entry dir with no .go files → hard error.
	if err := os.MkdirAll(filepath.Join(root, "empty"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := goClosure(root, "app", []byte("module example.com/app\n"), nil, []string{"../empty"}); err == nil {
		t.Fatal("empty entry dir accepted")
	}
	// go_mod without module directive → error.
	if _, err := goClosure(root, "app", []byte("// nothing\n"), nil, []string{"."}); err == nil {
		t.Fatal("module-less go.mod accepted")
	}
	// entry escaping the context root → error.
	if _, err := goClosure(root, "app", []byte("module example.com/app\n"), nil, []string{"../../up"}); err == nil {
		t.Fatal("escaping entry accepted")
	}
}

func TestGoClosureReviewRegressions(t *testing.T) {
	// (1) Dot-less local module paths (module api) resolve locally, not as
	// stdlib.
	root := t.TempDir()
	writeTree(t, root, map[string]string{
		"app/go.mod":   "module app\n\nreplace api => ../lib\n",
		"app/main.go":  "package main\n\nimport _ \"api/foo\"\n",
		"lib/go.mod":   "module api\n",
		"lib/foo/f.go": "package foo\n",
	})
	gomod, err := os.ReadFile(filepath.Join(root, "app/go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := goClosure(root, "app", gomod, nil, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got, "//lib/foo") {
		t.Fatalf("dot-less local module import not covered: %v", got)
	}

	// (2) go.work covers dir-relative (same base as its use-directive
	// targets).
	root2 := t.TempDir()
	writeTree(t, root2, map[string]string{
		"ws/go.work": "use (\n\t.\n\t../lib\n)\n",
		"ws/go.mod":  "module example.com/ws\n",
		"ws/main.go": "package main\n\nimport _ \"example.com/lib/x\"\n",
		"lib/go.mod": "module example.com/lib\n",
		"lib/x/x.go": "package x\n",
	})
	gm2, _ := os.ReadFile(filepath.Join(root2, "ws/go.mod"))
	gw2, _ := os.ReadFile(filepath.Join(root2, "ws/go.work"))
	got2, err := goClosure(root2, "ws", gm2, gw2, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(got2, "//ws/go.work") {
		t.Fatalf("go.work not covered at the consumer dir: %v", got2)
	}
	if !slices.Contains(got2, "//lib/x") {
		t.Fatalf("go.work use-sibling package not covered: %v", got2)
	}

	// (3) Empty entry list is a hard error.
	if _, err := goClosure(root, "app", gomod, nil, nil); err == nil ||
		!strings.Contains(err.Error(), "empty entry list") {
		t.Fatalf("empty entries: want hard error, got %v", err)
	}

	// (4) all:-prefixed and quoted embed patterns resolve; testdata of a
	// module-root package is covered.
	root3 := t.TempDir()
	writeTree(t, root3, map[string]string{
		"go.mod": "module example.com/e\n",
		"main.go": `package main

import "embed"

//go:embed all:static
var a embed.FS

//go:embed "assets/logo.txt"
var b string
`,
		"static/deep/x.txt": "x\n",
		"assets/logo.txt":   "logo\n",
		"testdata/tc.json":  "{}\n",
	})
	gm3, _ := os.ReadFile(filepath.Join(root3, "go.mod"))
	got3, err := goClosure(root3, "", gm3, nil, []string{"."})
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range []string{"//static", "//assets/logo.txt", "//testdata"} {
		if !slices.Contains(got3, w) {
			t.Fatalf("missing %s in %v", w, got3)
		}
	}
}
