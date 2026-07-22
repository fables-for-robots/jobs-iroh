package builddef

import (
	"fmt"
	"path"
	"regexp"
	"sort"
	"strings"
)

// PinnedCache is one build-cache declaration carried in Pinned (build-cache
// design §3/§4): a persistent read-write directory the runner mounts at Path
// inside the build sandbox, whose state lives under the mutable
// build-cache:<ID>:<platform> ref. Cache state is advisory only — it may
// change how fast an output is produced, never which output is correct.
type PinnedCache struct {
	Path string `cbor:"path"`
	ID   string `cbor:"id"`
}

// cacheIDRe: no ':' anywhere — ':' is the ref-name separator, so its absence
// from the id is what makes build-cache:<id>:<platform> unambiguous.
var cacheIDRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]*$`)

const maxCacheIDLen = 128

// reservedCachePrefixes are in-sandbox paths a cache may not claim or nest
// under: the content-addressed store, source, output, devices, proc, and the
// tmpfs scratch.
var reservedCachePrefixes = []string{"/jobs", "/build/src", "/build/out", "/dev", "/proc", "/tmp"}

// ValidateCacheID checks the recipe-chosen cache id (design §3).
func ValidateCacheID(id string) error {
	if len(id) > maxCacheIDLen {
		return fmt.Errorf("cache id %q: longer than %d bytes", id, maxCacheIDLen)
	}
	if !cacheIDRe.MatchString(id) {
		return fmt.Errorf("cache id %q: must match %s", id, cacheIDRe.String())
	}
	return nil
}

// ValidateCachePath checks the recipe-chosen in-sandbox mount path (design
// §3). "/build" itself is rejected (exact match only, not a prefix block):
// the runner rw-bind-mounts a cache at Path, and a cache at the bare sandbox
// work root would shadow both /build/src and /build/out (already reserved
// individually below) — children like /build/cache remain valid.
func ValidateCachePath(p string) error {
	if p == "" || p[0] != '/' {
		return fmt.Errorf("cache path %q: must be absolute", p)
	}
	if p != path.Clean(p) {
		return fmt.Errorf("cache path %q: must be clean (want %q)", p, path.Clean(p))
	}
	if p == "/" {
		return fmt.Errorf("cache path must not be %q", "/")
	}
	if p == "/build" {
		return fmt.Errorf("cache path %q: the sandbox work root is reserved", p)
	}
	for _, res := range reservedCachePrefixes {
		if p == res || strings.HasPrefix(p, res+"/") {
			return fmt.Errorf("cache path %q: %s is reserved", p, res)
		}
	}
	return nil
}

// ValidateCaches applies per-entry validation plus the cross-entry rules: no
// duplicate id or path, and no cache path nested under another cache path.
func ValidateCaches(cs []PinnedCache) error {
	ids := make(map[string]bool, len(cs))
	paths := make(map[string]bool, len(cs))
	for _, c := range cs {
		if err := ValidateCachePath(c.Path); err != nil {
			return err
		}
		if err := ValidateCacheID(c.ID); err != nil {
			return err
		}
		if ids[c.ID] {
			return fmt.Errorf("duplicate cache id %q", c.ID)
		}
		if paths[c.Path] {
			return fmt.Errorf("duplicate cache path %q", c.Path)
		}
		ids[c.ID] = true
		paths[c.Path] = true
	}
	for _, a := range cs {
		for _, b := range cs {
			if a.Path != b.Path && strings.HasPrefix(a.Path, b.Path+"/") {
				return fmt.Errorf("cache path %q nests inside cache path %q", a.Path, b.Path)
			}
		}
	}
	return nil
}

// CanonicalCaches returns a stable-ordered copy of cs sorted by Path (unique
// after ValidateCaches), nil for empty — canonical CBOR preserves array order,
// so an unsorted slice would break byte-identical re-pins (canon.go).
func CanonicalCaches(cs []PinnedCache) []PinnedCache {
	if len(cs) == 0 {
		return nil
	}
	out := append([]PinnedCache(nil), cs...)
	sort.SliceStable(out, func(a, b int) bool { return out[a].Path < out[b].Path })
	return out
}

// CacheRefName is the mutable, engine-signed ref holding a cache's last
// uploaded state (design §2). The id contains no ':' (ValidateCacheID), so the
// first ':' after the prefix unambiguously terminates it.
func CacheRefName(id, platform string) string {
	return "build-cache:" + id + ":" + platform
}

// ParseCacheRefName splits a build-cache:<id>:<platform> name; ok is false for
// any other shape (wrong prefix, empty platform, invalid id). The platform
// part is opaque here — the engine gate compares it against the build's
// placement platform.
func ParseCacheRefName(name string) (id, platform string, ok bool) {
	rest, found := strings.CutPrefix(name, "build-cache:")
	if !found {
		return "", "", false
	}
	id, platform, found = strings.Cut(rest, ":")
	if !found || platform == "" || ValidateCacheID(id) != nil {
		return "", "", false
	}
	return id, platform, true
}
