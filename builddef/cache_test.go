package builddef

import (
	"bytes"
	"testing"
)

func TestValidateCacheID(t *testing.T) {
	for _, ok := range []string{"golang-myproject-1.26", "a", "A.b_c-9"} {
		if err := ValidateCacheID(ok); err != nil {
			t.Errorf("%q: unexpected error %v", ok, err)
		}
	}
	long := make([]byte, 129)
	for i := range long {
		long[i] = 'a'
	}
	for _, bad := range []string{"", "-leading", ".leading", "has:colon", "has/slash", "has space", string(long)} {
		if err := ValidateCacheID(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestValidateCachePath(t *testing.T) {
	for _, ok := range []string{"/go-build-cache", "/caches/go", "/build/cache"} {
		if err := ValidateCachePath(ok); err != nil {
			t.Errorf("%q: unexpected error %v", ok, err)
		}
	}
	for _, bad := range []string{"", "relative", "/", "/build", "/jobs", "/jobs/store/x", "/build/src", "/build/out/x",
		"/dev/shm", "/proc/self", "/tmp", "/tmp/x", "/a/../b", "/a/"} {
		if err := ValidateCachePath(bad); err == nil {
			t.Errorf("%q: expected error", bad)
		}
	}
}

func TestValidateCaches_CrossEntry(t *testing.T) {
	if err := ValidateCaches([]PinnedCache{{Path: "/a", ID: "x"}, {Path: "/b", ID: "x"}}); err == nil {
		t.Error("duplicate id: expected error")
	}
	if err := ValidateCaches([]PinnedCache{{Path: "/a", ID: "x"}, {Path: "/a", ID: "y"}}); err == nil {
		t.Error("duplicate path: expected error")
	}
	if err := ValidateCaches([]PinnedCache{{Path: "/a", ID: "x"}, {Path: "/a/b", ID: "y"}}); err == nil {
		t.Error("nested paths: expected error")
	}
	if err := ValidateCaches([]PinnedCache{{Path: "/a", ID: "x"}, {Path: "/ab", ID: "y"}}); err != nil {
		t.Errorf("/a and /ab do not nest: %v", err)
	}
}

func TestCanonicalCaches(t *testing.T) {
	got := CanonicalCaches([]PinnedCache{{Path: "/z", ID: "b"}, {Path: "/a", ID: "a"}})
	if len(got) != 2 || got[0].Path != "/a" || got[1].Path != "/z" {
		t.Fatalf("not sorted by path: %+v", got)
	}
	if CanonicalCaches(nil) != nil {
		t.Fatal("empty input must return nil")
	}
}

func TestCacheRefNameRoundTrip(t *testing.T) {
	name := CacheRefName("go-1.26", "linux/arm64")
	if name != "build-cache:go-1.26:linux/arm64" {
		t.Fatalf("name = %q", name)
	}
	id, platform, ok := ParseCacheRefName(name)
	if !ok || id != "go-1.26" || platform != "linux/arm64" {
		t.Fatalf("parse = %q %q %v", id, platform, ok)
	}
	// Parser contract: prefix + valid id + non-empty remainder. The platform
	// part is opaque here (a shape like "build-cache:bad:id:x" parses with
	// platform "id:x" — the engine's platform-equality check rejects it).
	for _, bad := range []string{"build-output:xx", "build-cache:", "build-cache:noplatform",
		"build-cache::linux/arm64", "build-cache:has space:linux/arm64"} {
		if _, _, ok := ParseCacheRefName(bad); ok {
			t.Errorf("%q: expected not ok", bad)
		}
	}
}

// TestPinnedEncodingByteCompat proves cbor omitempty: a cache-less Pinned
// encodes byte-identical to the pre-Caches struct layout, so existing
// build-pinned:F refs stay stable and mixed-fleet re-pins agree (design §4).
func TestPinnedEncodingByteCompat(t *testing.T) {
	type legacyPinned struct {
		Inputs      []PinnedInput     `cbor:"inputs"`
		Env         map[string]string `cbor:"env"`
		Script      string            `cbor:"script"`
		RuntimeDeps [][]byte          `cbor:"runtimeDeps"`
	}
	in := []PinnedInput{{Name: "dep", Kind: "import", Definition: []byte{0xa0}}}
	env := map[string]string{"A": "b"}
	rt := [][]byte{{1, 2, 3}}
	newBytes, err := EncodePinned(Pinned{Inputs: in, Env: env, Script: "s", RuntimeDeps: rt})
	if err != nil {
		t.Fatal(err)
	}
	oldBytes, err := canonEnc.Marshal(legacyPinned{Inputs: in, Env: env, Script: "s", RuntimeDeps: rt})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(newBytes, oldBytes) {
		t.Fatalf("cache-less Pinned encoding changed:\nnew %x\nold %x", newBytes, oldBytes)
	}
	// And a Pinned WITH caches must decode its caches back.
	withCaches := Pinned{Script: "s", Caches: []PinnedCache{{Path: "/c", ID: "i"}}}
	b, err := EncodePinned(withCaches)
	if err != nil {
		t.Fatal(err)
	}
	got, err := DecodePinned(b)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Caches) != 1 || got.Caches[0] != (PinnedCache{Path: "/c", ID: "i"}) {
		t.Fatalf("caches round-trip: %+v", got.Caches)
	}
}
