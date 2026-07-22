package recipe

import (
	"strings"
	"testing"

	"github.com/fables-for-robots/jobs-iroh/builddef"
)

func evalCachesRecipe(t *testing.T, cachesLine string) (BuildResult, error) {
	t.Helper()
	recipeSrc := []byte(`
def build():
    return struct(
        inputs = {},
        env = {},
        script = "true",
        runtime_deps = [],
` + cachesLine + `
    )
`)
	return EvalBuild(cfg(t, MapSource{}), recipeSrc, nil)
}

func TestEvalBuild_CachesDecoded(t *testing.T) {
	res, err := evalCachesRecipe(t, `        caches = {"/z-cache": "id-z", "/a-cache": "id-a"},`)
	if err != nil {
		t.Fatal(err)
	}
	if err := res.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(res.Caches) != 2 {
		t.Fatalf("caches = %+v", res.Caches)
	}
	got := map[string]string{}
	for _, c := range res.Caches {
		got[c.Path] = c.ID
	}
	if got["/z-cache"] != "id-z" || got["/a-cache"] != "id-a" {
		t.Fatalf("caches = %+v", res.Caches)
	}
}

func TestEvalBuild_CachesAbsent(t *testing.T) {
	res, err := evalCachesRecipe(t, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Caches) != 0 {
		t.Fatalf("expected no caches, got %+v", res.Caches)
	}
}

func TestEvalBuild_CachesValidationRejects(t *testing.T) {
	cases := map[string]string{
		"relative path": `        caches = {"rel-cache": "ok-id"},`,
		"reserved path": `        caches = {"/jobs/store/x": "ok-id"},`,
		"bad id":        `        caches = {"/c": "has:colon"},`,
		"dup id":        `        caches = {"/c1": "same", "/c2": "same"},`,
		"nested paths":  `        caches = {"/c": "a", "/c/inner": "b"},`,
		"non-string id": `        caches = {"/c": 42},`,
	}
	for name, line := range cases {
		res, err := evalCachesRecipe(t, line)
		if err == nil {
			err = res.Validate()
		}
		if err == nil {
			t.Errorf("%s: expected an error", name)
		}
	}
}

func TestEvalBuild_CachesErrorMentionsCaches(t *testing.T) {
	res, err := evalCachesRecipe(t, `        caches = {"/c": "has:colon"},`)
	if err == nil {
		err = res.Validate()
	}
	if err == nil || !strings.Contains(err.Error(), "cache") {
		t.Fatalf("err = %v", err)
	}
	_ = builddef.PinnedCache{} // keep the import used if assertions change
}
