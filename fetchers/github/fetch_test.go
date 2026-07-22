package main

import (
	"archive/tar"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func gzTarball(t *testing.T) []byte {
	return buildTar(t, []tentry{
		{name: "owner-repo-sha/", typeflag: tar.TypeDir},
		{name: "owner-repo-sha/README.md", typeflag: tar.TypeReg, body: "hello\n"},
	})
}

func TestFetch_HappyPath(t *testing.T) {
	body := gzTarball(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/repos/o/r/tarball/main" {
			http.NotFound(w, r)
			return
		}
		w.Write(body)
	}))
	defer srv.Close()

	p, _ := parseParams([]byte(`{"owner":"o","repo":"r","ref":"main","apiBaseURL":"` + srv.URL + `"}`))
	host, _ := apiHost(p.APIBaseURL)
	rc, err := fetch(context.Background(), p, host, "")
	if err != nil {
		t.Fatal(err)
	}
	defer rc.Close()
	dest := t.TempDir()
	if err := extractTarball(rc, dest); err != nil {
		t.Fatal(err)
	}
	got, _ := os.ReadFile(filepath.Join(dest, "README.md"))
	if string(got) != "hello\n" {
		t.Fatalf("README.md=%q", got)
	}
}

func TestFetch_TokenAttachedOnScopeMatch(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write(gzTarball(t))
	}))
	defer srv.Close()

	p, _ := parseParams([]byte(`{"owner":"o","repo":"r","ref":"main","apiBaseURL":"` + srv.URL + `"}`))
	host, _ := apiHost(p.APIBaseURL)
	rc, err := fetch(context.Background(), p, host, "ghp_secret")
	if err != nil {
		t.Fatal(err)
	}
	rc.Close()
	if gotAuth != "Bearer ghp_secret" {
		t.Fatalf("auth header = %q", gotAuth)
	}
}

func TestFetch_TokenStrippedOnCrossHostRedirect(t *testing.T) {
	var downstreamAuth string
	downstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		downstreamAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
	}))
	defer downstream.Close()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, downstream.URL+"/codeload", http.StatusFound)
	}))
	defer api.Close()

	p, _ := parseParams([]byte(`{"owner":"o","repo":"r","ref":"main","apiBaseURL":"` + api.URL + `"}`))
	host, _ := apiHost(p.APIBaseURL) // token scoped to the API host only
	rc, err := fetch(context.Background(), p, host, "ghp_secret")
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, rc)
	rc.Close()
	if downstreamAuth != "" {
		t.Fatalf("token leaked to redirect target: %q", downstreamAuth)
	}
}

func TestFetch_NotFoundIsHard(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()
	p, _ := parseParams([]byte(`{"owner":"o","repo":"r","ref":"nope","apiBaseURL":"` + srv.URL + `"}`))
	host, _ := apiHost(p.APIBaseURL)
	_, err := fetch(context.Background(), p, host, "")
	if err == nil || isRetryable(err) {
		t.Fatalf("expected hard error, got %v", err)
	}
}

func TestRun_HappyPath(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(gzTarball(t))
	}))
	defer srv.Close()
	out := t.TempDir()
	env := map[string]string{
		"JOBS_FETCH_PARAMS": `{"owner":"o","repo":"r","ref":"main","apiBaseURL":"` + srv.URL + `"}`,
		"JOBS_OUTPUT_DIR":   out,
	}
	code := run(func(k string) string { return env[k] }, io.Discard)
	if code != exitOK {
		t.Fatalf("exit code = %d", code)
	}
	got, _ := os.ReadFile(filepath.Join(out, "README.md"))
	if string(got) != "hello\n" {
		t.Fatalf("README.md = %q", got)
	}
}

func TestRun_BadParamsIsHard(t *testing.T) {
	env := map[string]string{"JOBS_FETCH_PARAMS": `{}`, "JOBS_OUTPUT_DIR": t.TempDir()}
	if code := run(func(k string) string { return env[k] }, io.Discard); code != exitHard {
		t.Fatalf("exit code = %d, want %d", code, exitHard)
	}
}

func TestRun_ServerErrorIsRetryable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	env := map[string]string{
		"JOBS_FETCH_PARAMS": `{"owner":"o","repo":"r","ref":"main","apiBaseURL":"` + srv.URL + `"}`,
		"JOBS_OUTPUT_DIR":   t.TempDir(),
	}
	if code := run(func(k string) string { return env[k] }, io.Discard); code != exitRetryable {
		t.Fatalf("exit code = %d, want %d", code, exitRetryable)
	}
}

func TestRefPath(t *testing.T) {
	cases := map[string]string{
		"v1.2.3":      "v1.2.3",
		"feature/x":   "feature/x",
		"a b":         "a%20b",
		"feature/a b": "feature/a%20b",
	}
	for in, want := range cases {
		if got := refPath(in); got != want {
			t.Fatalf("refPath(%q)=%q want %q", in, got, want)
		}
	}
}

func resp(code int, headers map[string]string) *http.Response {
	h := http.Header{}
	for k, v := range headers {
		h.Set(k, v)
	}
	return &http.Response{StatusCode: code, Header: h}
}

func TestRun_AttachesScopedTokenFromSecretsFile(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Write(gzTarball(t))
	}))
	defer srv.Close()

	host, err := apiHost(srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	secretsJSON := `{"github-creds":{"scope":"` + host + `","secret":{"token":"ghp_run_test"}}}`
	secretsFile := filepath.Join(t.TempDir(), "secrets.json")
	if err := os.WriteFile(secretsFile, []byte(secretsJSON), 0o600); err != nil {
		t.Fatal(err)
	}

	out := t.TempDir()
	env := map[string]string{
		"JOBS_FETCH_PARAMS": `{"owner":"o","repo":"r","ref":"main","apiBaseURL":"` + srv.URL + `"}`,
		"JOBS_OUTPUT_DIR":   out,
		"JOBS_SECRETS_FILE": secretsFile,
	}
	code := run(func(k string) string { return env[k] }, io.Discard)
	if code != exitOK {
		t.Fatalf("exit code = %d, want %d", code, exitOK)
	}
	if gotAuth != "Bearer ghp_run_test" {
		t.Fatalf("Authorization header = %q, want %q", gotAuth, "Bearer ghp_run_test")
	}
}

func TestClassifyHTTP(t *testing.T) {
	cases := []struct {
		name      string
		resp      *http.Response
		wantErr   bool
		retryable bool
	}{
		{"ok200", resp(200, nil), false, false},
		{"notFound", resp(404, nil), true, false},
		{"unauth", resp(401, nil), true, false},
		{"forbiddenPerm", resp(403, nil), true, false},
		{"rateLimited", resp(403, map[string]string{"X-RateLimit-Remaining": "0"}), true, true},
		{"tooMany", resp(429, nil), true, true},
		{"serverErr", resp(503, nil), true, true},
		{"weird", resp(418, nil), true, false},
	}
	for _, c := range cases {
		err := classifyHTTP(c.resp)
		if c.wantErr != (err != nil) {
			t.Fatalf("%s: wantErr=%v err=%v", c.name, c.wantErr, err)
		}
		if err != nil && isRetryable(err) != c.retryable {
			t.Fatalf("%s: retryable=%v want %v", c.name, isRetryable(err), c.retryable)
		}
	}
}
