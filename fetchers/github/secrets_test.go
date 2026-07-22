package main

import "testing"

const sampleSecrets = `{
  "github-creds": {"scope":"api.github.com","secret":{"token":"ghp_match"}},
  "other":        {"scope":"example.com","secret":{"token":"ghp_other"}}
}`

func TestSelectToken_ScopeMatch(t *testing.T) {
	tok, ok, err := selectToken([]byte(sampleSecrets), "api.github.com")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || tok != "ghp_match" {
		t.Fatalf("ok=%v tok=%q", ok, tok)
	}
}

func TestSelectToken_NoScopeMatch(t *testing.T) {
	_, ok, err := selectToken([]byte(sampleSecrets), "ghe.internal")
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected no match")
	}
}

func TestSelectToken_EmptyTokenIsError(t *testing.T) {
	_, _, err := selectToken([]byte(`{"t":{"scope":"h","secret":{"token":""}}}`), "h")
	if err == nil {
		t.Fatal("expected error for empty token")
	}
}

func TestSelectToken_BadSecretShape(t *testing.T) {
	_, _, err := selectToken([]byte(`{"t":{"scope":"h","secret":"not-an-object"}}`), "h")
	if err == nil {
		t.Fatal("expected error decoding secret")
	}
}

func TestSelectToken_Deterministic(t *testing.T) {
	// Two tags share the matching scope; selection must be stable (sorted by tag).
	s := `{"b":{"scope":"h","secret":{"token":"B"}},"a":{"scope":"h","secret":{"token":"A"}}}`
	tok, ok, err := selectToken([]byte(s), "h")
	if err != nil || !ok || tok != "A" {
		t.Fatalf("ok=%v tok=%q err=%v", ok, tok, err)
	}
}
