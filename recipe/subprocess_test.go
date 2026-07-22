package recipe

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// writePlugin writes an executable ./plugin script into a fresh dir and returns it.
func writePlugin(t *testing.T, script string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "plugin"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return dir
}

// A plugin that echoes a fixed CBOR response. We avoid needing a CBOR encoder in
// the fixture by emitting pre-encoded bytes via printf of octal escapes.
// Response is CBOR for: ["ok"] => 0x81 0x62 'o' 'k'.
func TestSubprocessPlugin_RoundTrip(t *testing.T) {
	dir := writePlugin(t, "#!/bin/sh\n"+
		"cat >/dev/null\n"+ // drain the CBOR request on stdin
		"printf '\\201\\142ok'\n") // 0x81 0x62 6f 6b  => ["ok"]
	p := SubprocessPlugin{Dir: dir, SourceDir: t.TempDir()}
	resp, err := p.Call(map[string]any{"go_mod": []byte("module x\n")})
	if err != nil {
		t.Fatal(err)
	}
	list, ok := resp.([]any)
	if !ok || len(list) != 1 || list[0] != "ok" {
		t.Fatalf("resp=%#v", resp)
	}
}

func TestSubprocessPlugin_HardOnExit1(t *testing.T) {
	dir := writePlugin(t, "#!/bin/sh\ncat >/dev/null\necho bad >&2\nexit 1\n")
	p := SubprocessPlugin{Dir: dir, SourceDir: t.TempDir()}
	_, err := p.Call(map[string]any{})
	var pe *PluginError
	if !errors.As(err, &pe) || pe.Retryable || pe.ExitCode != 1 {
		t.Fatalf("want hard exit 1, got %v", err)
	}
}

func TestSubprocessPlugin_RetryableOnExit75(t *testing.T) {
	dir := writePlugin(t, "#!/bin/sh\ncat >/dev/null\nexit 75\n")
	p := SubprocessPlugin{Dir: dir, SourceDir: t.TempDir()}
	_, err := p.Call(map[string]any{})
	var pe *PluginError
	if !errors.As(err, &pe) || !pe.Retryable || pe.ExitCode != 75 {
		t.Fatalf("want retryable, got %v", err)
	}
}
