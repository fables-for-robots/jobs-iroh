package bootstrap

import (
	"strings"
	"testing"
)

func TestLoadBytes_ParsesEmbedded(t *testing.T) {
	const src = `
[[fetcher]]
name = "tarball+https"
platform = "linux/amd64"
source = "embedded"

[[fetcher]]
name = "github"
platform = "linux/arm64"
source = "embedded"
`
	m, err := LoadBytes([]byte(src))
	if err != nil {
		t.Fatalf("LoadBytes: %v", err)
	}
	if len(m.Fetchers) != 2 {
		t.Fatalf("want 2 fetchers, got %d", len(m.Fetchers))
	}
	if m.Fetchers[0].Source != SourceEmbedded || m.Fetchers[0].Platform != "linux/amd64" {
		t.Errorf("entry0: %+v", m.Fetchers[0])
	}
}

func TestValidate(t *testing.T) {
	cases := []struct {
		name string
		src  string
		want string // error substring; "" = valid
	}{
		{"valid embedded", `
[[fetcher]]
name = "shell"
platform = "linux/amd64"
source = "embedded"
`, ""},
		{"build entries are rejected", `
[[fetcher]]
name = "gomod"
source = "build"
build = { fetcher = "tarball+https", url = "https://example.test/g.tar.gz", sha256 = "abc" }
`, "not supported"},
		{"unknown source", `
[[fetcher]]
name = "x"
platform = "linux/amd64"
source = "wat"
`, "not supported"},
		{"empty name", `
[[fetcher]]
name = ""
platform = "linux/amd64"
source = "embedded"
`, "empty name"},
		{"missing platform", `
[[fetcher]]
name = "shell"
source = "embedded"
`, "requires a platform"},
		{"duplicate name+platform", `
[[fetcher]]
name = "shell"
platform = "linux/amd64"
source = "embedded"

[[fetcher]]
name = "shell"
platform = "linux/amd64"
source = "embedded"
`, "duplicate"},
	}
	for _, c := range cases {
		_, err := LoadBytes([]byte(c.src))
		if c.want == "" {
			if err != nil {
				t.Errorf("%s: unexpected error: %v", c.name, err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s: error %v does not contain %q", c.name, err, c.want)
		}
	}
}
