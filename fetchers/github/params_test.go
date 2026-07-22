package main

import (
	"errors"
	"testing"
)

func TestParseParams_Valid(t *testing.T) {
	p, err := parseParams([]byte(`{"owner":"draganm","repo":"jobs","ref":"v1.2.3"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.Owner != "draganm" || p.Repo != "jobs" || p.Ref != "v1.2.3" {
		t.Fatalf("got %+v", p)
	}
	if p.APIBaseURL != "https://api.github.com" {
		t.Fatalf("default base url: %q", p.APIBaseURL)
	}
}

func TestParseParams_CustomBase(t *testing.T) {
	p, err := parseParams([]byte(`{"owner":"o","repo":"r","ref":"x","apiBaseURL":"https://gh.example/api/v3"}`))
	if err != nil {
		t.Fatal(err)
	}
	if p.APIBaseURL != "https://gh.example/api/v3" {
		t.Fatalf("base url: %q", p.APIBaseURL)
	}
}

func TestParseParams_Invalid(t *testing.T) {
	for _, raw := range []string{
		`{"repo":"jobs","ref":"v1"}`,  // no owner
		`{"owner":"d","ref":"v1"}`,    // no repo
		`{"owner":"d","repo":"jobs"}`, // no ref
		``,                            // empty
		`{`,                           // malformed
	} {
		if _, err := parseParams([]byte(raw)); err == nil {
			t.Fatalf("expected error for %q", raw)
		}
	}
}

func TestApiHost(t *testing.T) {
	h, err := apiHost("https://api.github.com")
	if err != nil || h != "api.github.com" {
		t.Fatalf("h=%q err=%v", h, err)
	}
	if _, err := apiHost("::not a url"); err == nil {
		t.Fatal("expected error for bad url")
	}
}

func TestClassifiedErrors(t *testing.T) {
	if !isRetryable(retryErr("x")) {
		t.Fatal("retryErr should be retryable")
	}
	if isRetryable(hardErr("x")) {
		t.Fatal("hardErr should not be retryable")
	}
	if isRetryable(errors.New("plain")) {
		t.Fatal("plain error should not be retryable")
	}
}
