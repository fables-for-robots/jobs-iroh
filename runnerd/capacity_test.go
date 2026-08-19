package runnerd

// Capacity resolution: an explicit --size is a rung-shaped cap, while
// auto-detect hands the FULL reserve-adjusted machine capacity to admission —
// the ladder classifies jobs, not runners (multi-job-runner semantics; the
// floor-to-rung of the first port wasted everything above the top rung).

import (
	"log/slog"
	"slices"
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"

	"github.com/jobs-build/jobs-iroh/resources"
	"github.com/jobs-build/jobs-iroh/runner"
	"github.com/jobs-build/jobs-iroh/wire"
)

// startInProcNATS spins a DontListen NATS server and returns an in-process
// connection to it (newDaemon needs one for its JetStream handle).
func startInProcNATS(t *testing.T, name string) *nats.Conn {
	t.Helper()
	ns, err := natsserver.NewServer(&natsserver.Options{
		ServerName: name,
		DontListen: true,
	})
	if err != nil {
		t.Fatalf("new nats server: %v", err)
	}
	go ns.Start()
	t.Cleanup(ns.Shutdown)
	if !ns.ReadyForConnections(10 * time.Second) {
		t.Fatal("nats server not ready")
	}
	nc, err := nats.Connect("", nats.InProcessServer(ns))
	if err != nil {
		t.Fatalf("nats connect: %v", err)
	}
	t.Cleanup(nc.Close)
	return nc
}

func TestResolveCapacityExplicitRung(t *testing.T) {
	got, err := resolveCapacity("c1-m2", "", "", slog.Default())
	if err != nil {
		t.Fatalf("resolveCapacity: %v", err)
	}
	if want := wire.Class("c1-m2").Resources(); got != want {
		t.Fatalf("capacity = %v, want %v", got, want)
	}
	if _, err := resolveCapacity("c9-m99", "", "", slog.Default()); err == nil {
		t.Fatal("unknown rung must be rejected")
	}
}

func TestResolveCapacityAutoDetectIsFull(t *testing.T) {
	// Empty size must yield the detected (reserve-adjusted) capacity
	// verbatim — NOT floored to a ladder rung.
	want := runner.DetectCapacity("", "", slog.Default())
	got, err := resolveCapacity("", "", "", slog.Default())
	if err != nil {
		t.Fatalf("resolveCapacity: %v", err)
	}
	if got != want {
		t.Fatalf("capacity = %v, want detected %v", got, want)
	}
}

func TestDaemonDerivesLanesAndLabelFromCapacity(t *testing.T) {
	nc := startInProcNATS(t, "runnerd-capacity-test")

	// A 32-core / 125 GiB box after reserve: way above the ladder top.
	capacity := resources.Resources{CPUMilli: 28800, MemBytes: 113 << 30}
	d, err := newDaemon(daemonConfig{
		NC:       nc,
		ID:       "cap-test",
		Name:     "cap-test",
		Platform: testPlatform,
		Capacity: capacity,
	})
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}
	if !slices.Equal(d.classes, wire.Classes) {
		t.Fatalf("classes = %v, want the full ladder", d.classes)
	}
	// The display label is the biggest job class the runner accepts.
	if d.size != wire.Class("c4-m16") {
		t.Fatalf("size label = %q, want c4-m16", d.size)
	}
	// Admission must hold more than one top-rung job at once — the rung cap
	// of the first port allowed exactly one.
	top := wire.Class("c4-m16").Resources()
	if !d.adm.TryAcquire("job/a", top) || !d.adm.TryAcquire("job/b", top) {
		t.Fatal("full capacity must admit two c4-m16 jobs concurrently")
	}
}

func TestHelloAdvertisesFullCapacity(t *testing.T) {
	nc := startInProcNATS(t, "runnerd-hello-test")

	capacity := resources.Resources{CPUMilli: 28800, MemBytes: 113 << 30}
	d, err := newDaemon(daemonConfig{
		NC:       nc,
		ID:       "hello-test",
		Name:     "hello-test",
		Platform: testPlatform,
		Capacity: capacity,
	})
	if err != nil {
		t.Fatalf("new daemon: %v", err)
	}

	sub, err := nc.SubscribeSync(wire.SubjectRunnerHello)
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	d.publishHello()

	msg, err := sub.NextMsg(10 * time.Second)
	if err != nil {
		t.Fatalf("await hello: %v", err)
	}
	var hello wire.Hello
	if err := wire.Decode(msg.Data, &hello); err != nil {
		t.Fatalf("decode hello: %v", err)
	}
	// The rung is only the display label; the numbers must be the full
	// machine capacity, not the label's resources.
	if hello.Size != "c4-m16" {
		t.Fatalf("hello size = %q, want c4-m16", hello.Size)
	}
	if hello.CPUMilli != capacity.CPUMilli || hello.MemBytes != capacity.MemBytes {
		t.Fatalf("hello capacity = %dm/%dB, want %dm/%dB",
			hello.CPUMilli, hello.MemBytes, capacity.CPUMilli, capacity.MemBytes)
	}
}

func TestDaemonRejectsCapacityBelowLadder(t *testing.T) {
	nc := startInProcNATS(t, "runnerd-capacity-reject-test")

	_, err := newDaemon(daemonConfig{
		NC:       nc,
		ID:       "cap-test",
		Platform: testPlatform,
		Capacity: resources.Resources{CPUMilli: 100, MemBytes: 1 << 20},
	})
	if err == nil {
		t.Fatal("capacity fitting no ladder class must be rejected")
	}
}
