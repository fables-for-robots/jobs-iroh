//go:build !linux

package runner

// DevelopConfig configures a `jobs develop` / local `jobs run --source` run
// (see the Linux implementation). Declared off Linux too so the cross-platform
// run/develop entry points share one config type; the interactive develop
// shell itself (RunDevelop) arrives with the develop milestone and is
// Linux-only.
type DevelopConfig struct {
	SourceDir string
	Dir       string
	BuildFile string
	Platform  string
	Params    []byte
	ShellRef  string
	CacheDir  string
	Secrets   map[string]TagSecret
}
