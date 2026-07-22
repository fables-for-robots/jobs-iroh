package main

import (
	"encoding/json"
	"fmt"
	"sort"
)

// tagSecret mirrors the runner's secrets.json entry: a host scope plus opaque
// credential material (architecture/import.md §4).
type tagSecret struct {
	Scope  string          `json:"scope"`
	Secret json.RawMessage `json:"secret"`
}

// ghSecret is the GitHub credential shape carried in tagSecret.Secret.
type ghSecret struct {
	Token string `json:"token"`
}

// selectToken returns the token whose tag's scope exactly matches host. Selection
// is the single gate on credential use: the import cannot choose the tag, only the
// operator-set scope decides. When several tags share the matching scope, the
// lexicographically-first tag wins so the choice is deterministic.
func selectToken(secretsJSON []byte, host string) (string, bool, error) {
	var m map[string]tagSecret
	if err := json.Unmarshal(secretsJSON, &m); err != nil {
		return "", false, fmt.Errorf("decode secrets: %w", err)
	}
	tags := make([]string, 0, len(m))
	for t := range m {
		tags = append(tags, t)
	}
	sort.Strings(tags)
	for _, t := range tags {
		s := m[t]
		if s.Scope != host {
			continue
		}
		var gh ghSecret
		if err := json.Unmarshal(s.Secret, &gh); err != nil {
			return "", false, fmt.Errorf("decode secret for tag %q: %w", t, err)
		}
		if gh.Token == "" {
			return "", false, fmt.Errorf("empty token for tag %q", t)
		}
		return gh.Token, true, nil
	}
	return "", false, nil
}
