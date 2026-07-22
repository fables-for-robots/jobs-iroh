package main

import (
	"encoding/json"
	"fmt"
	"net/url"
)

const defaultAPIBaseURL = "https://api.github.com"

// Params are the fetcher inputs decoded from JOBS_FETCH_PARAMS.
type Params struct {
	Owner      string `json:"owner"`
	Repo       string `json:"repo"`
	Ref        string `json:"ref"`
	APIBaseURL string `json:"apiBaseURL"`
}

// parseParams decodes and validates the params JSON, defaulting APIBaseURL.
func parseParams(raw []byte) (Params, error) {
	var p Params
	if len(raw) == 0 {
		return p, fmt.Errorf("empty params")
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return p, fmt.Errorf("decode params: %w", err)
	}
	if p.Owner == "" {
		return p, fmt.Errorf("owner is required")
	}
	if p.Repo == "" {
		return p, fmt.Errorf("repo is required")
	}
	if p.Ref == "" {
		return p, fmt.Errorf("ref is required")
	}
	if p.APIBaseURL == "" {
		p.APIBaseURL = defaultAPIBaseURL
	}
	return p, nil
}

// apiHost returns the host (host:port) of a base URL — the host a token's scope
// must match before it may be attached.
func apiHost(base string) (string, error) {
	u, err := url.Parse(base)
	if err != nil {
		return "", err
	}
	if u.Host == "" {
		return "", fmt.Errorf("no host in %q", base)
	}
	return u.Host, nil
}
