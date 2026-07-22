package bootstrap

import "testing"

// hasSeed reports whether the manifest lists an embedded entry for name on any
// platform.
func hasSeed(m *Manifest, name string) bool {
	for _, e := range m.Fetchers {
		if e.Name == name && e.Source == SourceEmbedded {
			return true
		}
	}
	return false
}

// TestRepoManifestValidates guards the checked-in fetchers.toml against schema
// regressions: seed leaves only (build entries are a parse error since the
// recipe-declared-fetchers design).
func TestRepoManifestValidates(t *testing.T) {
	m, err := Load("../fetchers.toml")
	if err != nil {
		t.Fatalf("load ../fetchers.toml: %v", err)
	}
	for _, name := range []string{"tarball+https", "shell", "hostmusl", "github"} {
		if !hasSeed(m, name) {
			t.Errorf("manifest missing the %s seed leaf", name)
		}
	}
}

// TestEmbeddedManifestValidates guards the binary-embedded copy.
func TestEmbeddedManifestValidates(t *testing.T) {
	m, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("load embedded manifest: %v", err)
	}
	for _, name := range []string{"tarball+https", "shell", "hostmusl", "github"} {
		if !hasSeed(m, name) {
			t.Errorf("embedded manifest missing the %s seed leaf", name)
		}
	}
}
