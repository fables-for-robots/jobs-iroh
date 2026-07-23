package registryd

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	v1 "github.com/google/go-containerregistry/pkg/v1"
)

func testDigest(t *testing.T, content []byte) v1.Hash {
	t.Helper()
	sum := sha256.Sum256(content)
	return v1.Hash{Algorithm: "sha256", Hex: hex.EncodeToString(sum[:])}
}

func TestBlobCachePutVerifiesDigest(t *testing.T) {
	b := &blobCache{dir: t.TempDir(), log: slog.Default()}
	content := []byte("hello blob")
	d := testDigest(t, content)

	if err := b.put(d, bytes.NewReader(content)); err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := b.get(d)
	if err != nil || !bytes.Equal(got, content) {
		t.Fatalf("get = %q, %v", got, err)
	}

	// Wrong content under a digest never lands.
	wrong := testDigest(t, []byte("other"))
	if err := b.put(wrong, bytes.NewReader(content)); err == nil {
		t.Fatal("put with mismatched digest must fail")
	}
	if b.has(wrong) {
		t.Fatal("mismatched blob must not exist")
	}
	// No temp litter after the failed put.
	ents, err := os.ReadDir(filepath.Join(b.dir, "sha256"))
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != 1 {
		t.Fatalf("want exactly the one good blob on disk, got %d entries", len(ents))
	}
}

func TestBlobCacheSweepExpiresByLastRead(t *testing.T) {
	b := &blobCache{dir: t.TempDir(), log: slog.Default()}
	now := time.Now()
	ttl := time.Hour

	old := []byte("old blob")
	fresh := []byte("fresh blob")
	oldD, freshD := testDigest(t, old), testDigest(t, fresh)
	for d, c := range map[v1.Hash][]byte{oldD: old, freshD: fresh} {
		if err := b.put(d, bytes.NewReader(c)); err != nil {
			t.Fatal(err)
		}
	}
	// Age both past the TTL, then "read" one: touch keeps it alive.
	past := now.Add(-2 * ttl)
	for _, d := range []v1.Hash{oldD, freshD} {
		if err := os.Chtimes(b.path(d), past, past); err != nil {
			t.Fatal(err)
		}
	}
	b.touch(freshD)

	// An abandoned temp file also goes.
	tmp := filepath.Join(b.dir, "sha256", "tmp-crashed")
	if err := os.WriteFile(tmp, []byte("partial"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chtimes(tmp, past, past); err != nil {
		t.Fatal(err)
	}

	removed, bytesFreed := b.sweep(ttl, now)
	if removed != 2 {
		t.Errorf("removed = %d, want 2 (stale blob + temp)", removed)
	}
	if bytesFreed <= 0 {
		t.Errorf("bytes freed = %d, want > 0", bytesFreed)
	}
	if b.has(oldD) {
		t.Error("stale blob survived the sweep")
	}
	if !b.has(freshD) {
		t.Error("recently read blob must survive the sweep")
	}
	if _, err := os.Stat(tmp); !os.IsNotExist(err) {
		t.Error("abandoned temp file survived the sweep")
	}
}

func TestParseHexKey(t *testing.T) {
	for _, s := range []string{"", "abc", "not-a-key", "ABCDEF"} {
		if _, ok := parseHexKey(s); ok {
			t.Errorf("parseHexKey(%q) accepted", s)
		}
	}
	// Uppercase hex of the right length is rejected (repo names are lowercase).
	up := "00" + string(bytes.Repeat([]byte("A"), 62))
	if _, ok := parseHexKey(up); ok {
		t.Error("uppercase hex accepted")
	}
}
