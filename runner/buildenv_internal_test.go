//go:build linux

package runner

import (
	"fmt"
	"strings"
	"testing"
)

// A build with thousands of inputs produces a JOBS_DEPS JSON beyond the
// kernel's MAX_ARG_STRLEN (~128KiB per env string), which would fail the
// sandbox child's execve with E2BIG. The deps map must therefore ride in a
// file (JOBS_DEPS_FILE, always present) with the inline env var only kept for
// small maps.
func TestBuildEnvSpillsLargeJobsDeps(t *testing.T) {
	small := BuildSpec{JobsDeps: map[string]string{"a": "/jobs/store/x"}}
	env := buildEnv(small)
	if !envHasPrefix(env, "JOBS_DEPS=") {
		t.Fatal("small deps: JOBS_DEPS must stay inline")
	}
	if !envHasPrefix(env, "JOBS_DEPS_FILE="+sandboxJobsDepsFile) {
		t.Fatal("small deps: JOBS_DEPS_FILE must be set too")
	}

	big := BuildSpec{JobsDeps: map[string]string{}}
	for i := 0; i < 3000; i++ {
		big.JobsDeps[fmt.Sprintf("mod_%04d", i)] = "/jobs/store/" + strings.Repeat("f", 64)
	}
	env = buildEnv(big)
	if envHasPrefix(env, "JOBS_DEPS=") {
		t.Fatal("big deps: inline JOBS_DEPS would E2BIG the child execve")
	}
	if !envHasPrefix(env, "JOBS_DEPS_FILE="+sandboxJobsDepsFile) {
		t.Fatal("big deps: JOBS_DEPS_FILE must be set")
	}
}

func envHasPrefix(env []string, prefix string) bool {
	for _, e := range env {
		if strings.HasPrefix(e, prefix) {
			return true
		}
	}
	return false
}
