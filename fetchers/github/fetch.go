package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// refPath URL-escapes each segment of a git ref but keeps "/" literal, so branch
// names like "feature/x" stay valid in the tarball API path.
func refPath(ref string) string {
	parts := strings.Split(ref, "/")
	for i, p := range parts {
		parts[i] = url.PathEscape(p)
	}
	return strings.Join(parts, "/")
}

// fetch performs GET {apiBaseURL}/repos/{owner}/{repo}/tarball/{ref}. When token
// is non-empty it is attached as a Bearer credential and stripped on any redirect
// whose host differs from the credential's scope host. Returns the 2xx body (the
// caller must Close it) or a classified error.
func fetch(ctx context.Context, p Params, host, token string) (io.ReadCloser, error) {
	u := fmt.Sprintf("%s/repos/%s/%s/tarball/%s",
		strings.TrimRight(p.APIBaseURL, "/"),
		url.PathEscape(p.Owner),
		url.PathEscape(p.Repo),
		refPath(p.Ref),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, hardErr("build request: %v", err)
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("X-GitHub-Api-Version", "2022-11-28")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}

	client := &http.Client{
		CheckRedirect: func(r *http.Request, via []*http.Request) error {
			if r.URL.Host != host {
				r.Header.Del("Authorization")
			}
			if len(via) >= 10 {
				return errors.New("stopped after 10 redirects")
			}
			return nil
		},
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, retryErr("http request: %v", err)
	}
	if err := classifyHTTP(resp); err != nil {
		resp.Body.Close()
		return nil, err
	}
	return resp.Body, nil
}

// classifyHTTP maps a tarball-endpoint response to nil (2xx) or a classified
// failure. 404/401/permission-403 are permanent; rate-limit-403, 429, and 5xx are
// transient (architecture/import.md §3.3, design §8).
func classifyHTTP(resp *http.Response) error {
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return hardErr("github: 404 not found (repo or ref)")
	case resp.StatusCode == http.StatusUnauthorized:
		return hardErr("github: 401 unauthorized (bad token)")
	case resp.StatusCode == http.StatusForbidden:
		if resp.Header.Get("X-RateLimit-Remaining") == "0" {
			return retryErr("github: 403 rate limited")
		}
		return hardErr("github: 403 forbidden (permission denied)")
	case resp.StatusCode == http.StatusTooManyRequests:
		return retryErr("github: 429 too many requests")
	case resp.StatusCode >= 500:
		return retryErr("github: %d server error", resp.StatusCode)
	default:
		return hardErr("github: unexpected status %d", resp.StatusCode)
	}
}
