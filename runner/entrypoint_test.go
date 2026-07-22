package runner

import "testing"

func TestDecodeEntrypoint(t *testing.T) {
	ep, err := decodeEntrypoint([]byte(`{"command":"bin/app","args":["-x"],"env":{"A":"b"}}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ep.Command != "bin/app" {
		t.Errorf("command = %q, want bin/app", ep.Command)
	}
	if len(ep.Args) != 1 || ep.Args[0] != "-x" {
		t.Errorf("args = %v, want [-x]", ep.Args)
	}
	if ep.Env["A"] != "b" {
		t.Errorf("env[A] = %q, want b", ep.Env["A"])
	}
}

func TestDecodeEntrypointDefaults(t *testing.T) {
	ep, err := decodeEntrypoint([]byte(`{"command":"app"}`))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if ep.Args == nil || len(ep.Args) != 0 {
		t.Errorf("args default = %v, want []", ep.Args)
	}
	if ep.Env == nil || len(ep.Env) != 0 {
		t.Errorf("env default = %v, want {}", ep.Env)
	}
}

func TestDecodeEntrypointMissingCommand(t *testing.T) {
	if _, err := decodeEntrypoint([]byte(`{"args":[]}`)); err == nil {
		t.Fatal("expected error for missing command")
	}
}

func TestDecodeEntrypointBadJSON(t *testing.T) {
	if _, err := decodeEntrypoint([]byte(`{not json`)); err == nil {
		t.Fatal("expected error for bad json")
	}
}
