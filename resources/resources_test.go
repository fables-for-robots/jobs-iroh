package resources

import "testing"

func TestParseCPU(t *testing.T) {
	cases := map[string]int64{"200m": 200, "0.2": 200, "1": 1000, "1000m": 1000, "2": 2000, "500m": 500}
	for in, want := range cases {
		got, err := ParseCPU(in)
		if err != nil || got != want {
			t.Fatalf("ParseCPU(%q)=%d,%v want %d", in, got, err, want)
		}
	}
	if _, err := ParseCPU("nonsense"); err == nil {
		t.Fatal("expected error for nonsense")
	}
	if _, err := ParseCPU(""); err == nil {
		t.Fatal("expected error for empty")
	}
}

func TestParseMem(t *testing.T) {
	cases := map[string]int64{
		"512Mi": 512 << 20, "1Gi": 1 << 30, "1024": 1024, "2Gi": 2 << 30,
		"1Ki": 1 << 10, "1Ti": 1 << 40, "10M": 10_000_000, "1G": 1_000_000_000,
	}
	for in, want := range cases {
		got, err := ParseMem(in)
		if err != nil || got != want {
			t.Fatalf("ParseMem(%q)=%d,%v want %d", in, got, err, want)
		}
	}
	if _, err := ParseMem("nope"); err == nil {
		t.Fatal("expected error for nope")
	}
}

func TestMaxFits(t *testing.T) {
	a := Resources{CPUMilli: 200, MemBytes: 512 << 20}
	b := Resources{CPUMilli: 1000, MemBytes: 256 << 20}
	if got := a.Max(b); got != (Resources{1000, 512 << 20}) {
		t.Fatalf("Max=%+v", got)
	}
	capacity := Resources{CPUMilli: 4000, MemBytes: 8 << 30}
	if !(Resources{2000, 4 << 30}).Fits(capacity) {
		t.Fatal("should fit")
	}
	if (Resources{5000, 1 << 30}).Fits(capacity) {
		t.Fatal("cpu should not fit")
	}
	if (Resources{1000, 9 << 30}).Fits(capacity) {
		t.Fatal("mem should not fit")
	}
}

func TestAddSubIsZero(t *testing.T) {
	a := Resources{CPUMilli: 1000, MemBytes: 1 << 30}
	b := Resources{CPUMilli: 500, MemBytes: 512 << 20}
	if got := a.Add(b); got != (Resources{1500, (1 << 30) + (512 << 20)}) {
		t.Fatalf("Add=%+v", got)
	}
	if got := a.Sub(b); got != (Resources{500, 512 << 20}) {
		t.Fatalf("Sub=%+v", got)
	}
	if !(Resources{}).IsZero() {
		t.Fatal("zero value should be zero")
	}
	if a.IsZero() {
		t.Fatal("nonzero should not be zero")
	}
}

func TestDefaultFor(t *testing.T) {
	if DefaultFor(KindImport) != DefaultImport {
		t.Fatal("import default")
	}
	if DefaultFor(KindBuild) != DefaultBuild {
		t.Fatal("build default")
	}
	for _, k := range []string{KindBuildFrom, KindBuildPluginResolve, KindBuildPin} {
		if DefaultFor(k) != DefaultLightBuild {
			t.Fatalf("light default for %s", k)
		}
	}
	if DefaultFor("weird") != DefaultLightBuild {
		t.Fatal("unknown kind should fall back to light default")
	}
}
