package registryd

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/fables-for-robots/amber-store-core/key"
	v1 "github.com/google/go-containerregistry/pkg/v1"

	"github.com/fables-for-robots/jobs-iroh/runner"
)

// imageRecord is the tiny durable record of an assembled image, one JSON file
// per K under <data-dir>/repos. It maps the repository to its manifest and
// remembers F, so a cache-expired image reassembles without asking the server
// for the K→F ref again. Records are not swept — the blobs are the disk
// weight, the records are the memory.
type imageRecord struct {
	K         string   `json:"k"`
	F         string   `json:"f"`
	Platform  string   `json:"platform"`
	Manifest  string   `json:"manifest"`  // manifest digest, "sha256:<hex>"
	MediaType string   `json:"mediaType"` // manifest media type
	Blobs     []string `json:"blobs"`     // config + layer digests the manifest references
	CreatedNs int64    `json:"createdNs"`
}

// imageFor returns a servable record for K: the cached one when every blob is
// still present, otherwise a (re)assembly. Assembly is singleflighted per K
// and runs under the daemon context, so a departing HTTP client neither
// duplicates nor cancels the work its neighbours are waiting on.
func (r *registry) imageFor(reqCtx context.Context, k key.Key) (imageRecord, error) {
	if rec, ok := r.readRecord(k.String()); ok && r.recordComplete(rec) {
		return rec, nil
	}
	if r.runCtx.Err() != nil {
		return imageRecord{}, fmt.Errorf("%w: registry shutting down", errUpstream)
	}
	ch := r.group.DoChan("assemble:"+k.String(), func() (any, error) {
		// Tracked so Run can wait assemblies out before closing the store.
		// (An assembly scheduled but not yet registered when Wait runs dies
		// at its first store/ctx use — the guard above keeps that window to
		// goroutine-startup scale.)
		r.assemblies.Add(1)
		defer r.assemblies.Done()
		return r.assembleAndCache(r.runCtx, k)
	})
	select {
	case res := <-ch:
		if res.Err != nil {
			return imageRecord{}, res.Err
		}
		return res.Val.(imageRecord), nil
	case <-reqCtx.Done():
		return imageRecord{}, reqCtx.Err()
	}
}

// assembleAndCache resolves build K (syncing from the jobs-server as needed),
// assembles its two-layer OCI image and writes every blob plus the record.
func (r *registry) assembleAndCache(ctx context.Context, k key.Key) (imageRecord, error) {
	var fHint key.Key
	if rec, ok := r.readRecord(k.String()); ok {
		fHint, _ = parseHexKey(rec.F)
	}
	res, err := r.resolveBuild(ctx, k, fHint)
	if err != nil {
		return imageRecord{}, err
	}

	img, err := runner.AssembleOCIImage(ctx, r.st, res.artifact, res.deps, res.shell, res.ep, res.platform)
	if err != nil {
		return imageRecord{}, fmt.Errorf("assemble image for %s: %w", k.String(), err)
	}

	layers, err := img.Layers()
	if err != nil {
		return imageRecord{}, err
	}
	cfgDigest, err := img.ConfigName()
	if err != nil {
		return imageRecord{}, err
	}
	blobs := []string{cfgDigest.String()}
	for _, l := range layers {
		d, err := l.Digest()
		if err != nil {
			return imageRecord{}, err
		}
		rc, err := l.Compressed()
		if err != nil {
			return imageRecord{}, err
		}
		err = r.blobs.put(d, rc)
		rc.Close()
		if err != nil {
			return imageRecord{}, fmt.Errorf("cache layer %s: %w", d.String(), err)
		}
		blobs = append(blobs, d.String())
	}

	rawCfg, err := img.RawConfigFile()
	if err != nil {
		return imageRecord{}, err
	}
	if err := r.blobs.put(cfgDigest, bytes.NewReader(rawCfg)); err != nil {
		return imageRecord{}, fmt.Errorf("cache config: %w", err)
	}

	manDigest, err := img.Digest()
	if err != nil {
		return imageRecord{}, err
	}
	rawMan, err := img.RawManifest()
	if err != nil {
		return imageRecord{}, err
	}
	if err := r.blobs.put(manDigest, bytes.NewReader(rawMan)); err != nil {
		return imageRecord{}, fmt.Errorf("cache manifest: %w", err)
	}
	mt, err := img.MediaType()
	if err != nil {
		return imageRecord{}, err
	}

	rec := imageRecord{
		K:         k.String(),
		F:         res.f.String(),
		Platform:  res.platform,
		Manifest:  manDigest.String(),
		MediaType: string(mt),
		Blobs:     blobs,
		CreatedNs: time.Now().UnixNano(),
	}
	if err := r.writeRecord(rec); err != nil {
		return imageRecord{}, err
	}
	r.log.Info("image assembled", "k", k.String(), "f", res.f.String(),
		"manifest", rec.Manifest, "deps", len(res.deps), "platform", res.platform)
	return rec, nil
}

// recordComplete reports whether every blob the record's manifest references
// (and the manifest itself) is still in the cache.
func (r *registry) recordComplete(rec imageRecord) bool {
	for _, ds := range append([]string{rec.Manifest}, rec.Blobs...) {
		d, err := v1.NewHash(ds)
		if err != nil || !r.blobs.has(d) {
			return false
		}
	}
	return true
}

// touchRecord bumps the record's blobs as read: serving a manifest is proof
// the image is alive, so its layers should not expire out from under the
// client about to fetch them.
func (r *registry) touchRecord(rec imageRecord) {
	for _, ds := range append([]string{rec.Manifest}, rec.Blobs...) {
		if d, err := v1.NewHash(ds); err == nil {
			r.blobs.touch(d)
		}
	}
}

func (r *registry) recordPath(kHex string) string {
	return filepath.Join(r.reposDir, kHex+".json")
}

func (r *registry) readRecord(kHex string) (imageRecord, bool) {
	b, err := os.ReadFile(r.recordPath(kHex))
	if err != nil {
		return imageRecord{}, false
	}
	var rec imageRecord
	if err := json.Unmarshal(b, &rec); err != nil || rec.K != kHex {
		return imageRecord{}, false
	}
	return rec, true
}

// writeRecord persists the record atomically (temp + rename in-dir).
func (r *registry) writeRecord(rec imageRecord) error {
	b, err := json.Marshal(rec)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(r.reposDir, "tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), r.recordPath(rec.K))
}

// listRecords returns every stored image record, K-sorted (ReadDir order).
func (r *registry) listRecords() []imageRecord {
	entries, err := os.ReadDir(r.reposDir)
	if err != nil {
		return nil
	}
	out := make([]imageRecord, 0, len(entries))
	for _, e := range entries {
		kHex, ok := strings.CutSuffix(e.Name(), ".json")
		if !ok {
			continue
		}
		if rec, ok := r.readRecord(kHex); ok {
			out = append(out, rec)
		}
	}
	return out
}

// findRecordByDigest locates the image whose manifest references digest d —
// the recovery path when a client asks for a blob the sweep already deleted.
func (r *registry) findRecordByDigest(d v1.Hash) (imageRecord, bool) {
	ds := d.String()
	for _, rec := range r.listRecords() {
		if rec.Manifest == ds {
			return rec, true
		}
		for _, b := range rec.Blobs {
			if b == ds {
				return rec, true
			}
		}
	}
	return imageRecord{}, false
}

// findRecordByManifest locates the image whose MANIFEST digest is d — the
// gate for the manifests-by-digest route, so only actual manifests (small
// JSON, safe to buffer) are served there and layer digests 404.
func (r *registry) findRecordByManifest(d v1.Hash) (imageRecord, bool) {
	ds := d.String()
	for _, rec := range r.listRecords() {
		if rec.Manifest == ds {
			return rec, true
		}
	}
	return imageRecord{}, false
}

// parseHexKey parses a 64-char lowercase-hex store key (the repository-name
// form of K).
func parseHexKey(s string) (key.Key, bool) {
	if len(s) != 64 || strings.ToLower(s) != s {
		return key.Key{}, false
	}
	raw, err := hex.DecodeString(s)
	if err != nil {
		return key.Key{}, false
	}
	k, err := key.Parse(raw)
	if err != nil {
		return key.Key{}, false
	}
	return k, true
}
