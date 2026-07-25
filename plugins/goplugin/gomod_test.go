package main

import (
	"slices"
	"testing"
)

func TestRelDirectives(t *testing.T) {
	gomod := []byte(`
module example.com/services/api

go 1.24

require (
	example.com/lib/common v0.0.0
	github.com/real/dep v1.2.3
)

replace example.com/lib/common => ../../lib/common

replace (
	example.com/lib/other v1.0.0 => ../../lib/other // trailing comment
	github.com/pinned/thing => github.com/fork/thing v1.1.0
	example.com/local => ./vendored
)
`)
	got := relDirectives(gomod)
	want := []string{"../../lib/common", "../../lib/other", "./vendored"}
	if !slices.Equal(got, want) {
		t.Errorf("relDirectives = %v, want %v", got, want)
	}
}

func TestRelDirectivesGoWork(t *testing.T) {
	gowork := []byte(`
go 1.24

use (
	./services/api
	./lib/common
)

use ../outside
`)
	got := relDirectives(gowork)
	want := []string{"../outside", "./lib/common", "./services/api"}
	if !slices.Equal(got, want) {
		t.Errorf("relDirectives(go.work) = %v, want %v", got, want)
	}
}

func TestSiblingSources(t *testing.T) {
	rels := []string{"../../lib/common", "../../lib/other", "./vendored", "../api-sibling"}
	got := siblingSources(rels, "services/api")
	// ./vendored resolves inside the build dir — dropped (dir seed covers it).
	want := []string{"//lib/common", "//lib/other", "//services/api-sibling"}
	if !slices.Equal(got, want) {
		t.Errorf("siblingSources = %v, want %v", got, want)
	}
	// Root-dir consumer (go.work at the root): use-paths resolve directly.
	got = siblingSources([]string{"./lib/common", "./services/api"}, "")
	want = []string{"//lib/common", "//services/api"}
	if !slices.Equal(got, want) {
		t.Errorf("siblingSources(root) = %v, want %v", got, want)
	}
}
