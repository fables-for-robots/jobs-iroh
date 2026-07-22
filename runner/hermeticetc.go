package runner

import (
	"fmt"
	"os"
	"path/filepath"
)

// writeHermeticEtc stages the minimal resolver files every hermetic sandbox
// gets (issue #99), the same guarantee Nix's build sandbox makes:
//
//   - /etc/hosts answers "localhost" (v4+v6) instantly — loopback exists in
//     the sandbox's own netns, so a connect that follows fails fast with
//     ECONNREFUSED, which is the failure programs are written to expect;
//   - /etc/resolv.conf exists and is EMPTY, so any other lookup fails fast
//     ("no nameservers") instead of waiting on a resolver that cannot exist
//     under net=none.
//
// Without these, resolver code paths can hang FOREVER: pg 1.6 resolves
// database.yml's host itself (Ruby-side getaddrinfo) before connecting and
// parked on a UDP socket nothing would ever answer — a gitlab rails boot
// froze at zero CPU while heartbeats kept the job alive indefinitely.
//
// Written at assembly time, before the sandbox starts, into newRoot/etc —
// they are plain files in the sandbox root, not mounts.
func writeHermeticEtc(newRoot string) error {
	etc := filepath.Join(newRoot, "etc")
	if err := os.MkdirAll(etc, 0o755); err != nil {
		return fmt.Errorf("mkdir sandbox /etc: %w", err)
	}
	hosts := "127.0.0.1 localhost\n::1 localhost\n"
	if err := os.WriteFile(filepath.Join(etc, "hosts"), []byte(hosts), 0o644); err != nil {
		return fmt.Errorf("write sandbox /etc/hosts: %w", err)
	}
	if err := os.WriteFile(filepath.Join(etc, "resolv.conf"), nil, 0o644); err != nil {
		return fmt.Errorf("write sandbox /etc/resolv.conf: %w", err)
	}
	return nil
}
