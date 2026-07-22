package recipe

import "testing"

func TestBuildResourcesDecoded(t *testing.T) {
	src := []byte(`
def build():
    return struct(
        inputs = {},
        env = {},
        script = "true",
        runtime_deps = [],
        resources = struct(cpu = "2", memory = "4Gi"),
    )
`)
	res, err := EvalBuild(cfg(t, MapSource{}), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Resources == nil || res.Resources.CPUMilli != 2000 || res.Resources.MemBytes != 4<<30 {
		t.Fatalf("resources: %+v", res.Resources)
	}
}

func TestBuildResourcesOptional(t *testing.T) {
	src := []byte(`
def build():
    return struct(inputs={}, env={}, script="true", runtime_deps=[])
`)
	res, err := EvalBuild(cfg(t, MapSource{}), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Resources != nil {
		t.Fatalf("expected nil resources, got %+v", res.Resources)
	}
}

func TestBuildResourcesPartial(t *testing.T) {
	// Only memory set; cpu stays zero.
	src := []byte(`
def build():
    return struct(
        inputs = {}, env = {}, script = "true", runtime_deps = [],
        resources = struct(memory = "512Mi"),
    )
`)
	res, err := EvalBuild(cfg(t, MapSource{}), src, nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.Resources == nil || res.Resources.CPUMilli != 0 || res.Resources.MemBytes != 512<<20 {
		t.Fatalf("resources: %+v", res.Resources)
	}
}

func TestBuildResourcesBadValue(t *testing.T) {
	src := []byte(`
def build():
    return struct(
        inputs = {}, env = {}, script = "true", runtime_deps = [],
        resources = struct(cpu = "not-a-number"),
    )
`)
	if _, err := EvalBuild(cfg(t, MapSource{}), src, nil); err == nil {
		t.Fatal("expected error for bad cpu value")
	}
}
