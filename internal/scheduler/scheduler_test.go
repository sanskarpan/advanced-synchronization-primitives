package scheduler

import (
	"testing"
	"time"
)

func TestSchedulerRegisterUnregister(t *testing.T) {
	s := NewScheduler()
	s.Start()
	defer s.Stop()

	s.RegisterPrimitive("p1", TypeMutex, "my-mutex", nil)
	prims := s.GetPrimitives()
	if _, ok := prims["p1"]; !ok {
		t.Error("primitive p1 should be registered")
	}

	s.UnregisterPrimitive("p1")
	prims = s.GetPrimitives()
	if _, ok := prims["p1"]; ok {
		t.Error("primitive p1 should be unregistered")
	}
}

func TestSchedulerRegisterUnregisterGoroutine(t *testing.T) {
	s := NewScheduler()
	s.Start()
	defer s.Stop()

	s.RegisterGoroutine(1, "g1")
	goroutines := s.GetGoroutines()
	if _, ok := goroutines[1]; !ok {
		t.Error("goroutine 1 should be registered")
	}

	s.UnregisterGoroutine(1)
	goroutines = s.GetGoroutines()
	if _, ok := goroutines[1]; ok {
		t.Error("goroutine 1 should be pruned after unregister (R5)")
	}
}

func TestSchedulerUpdateStats(t *testing.T) {
	s := NewScheduler()
	s.Start()
	defer s.Stop()

	s.RegisterPrimitive("p1", TypeSemaphore, "sem", map[string]int{"capacity": 5})
	s.UpdatePrimitiveStats("p1", map[string]int{"capacity": 10})

	prims := s.GetPrimitives()
	prim, ok := prims["p1"]
	if !ok {
		t.Fatal("primitive p1 not found")
	}

	stats, ok := prim.Stats.(map[string]int)
	if !ok {
		t.Fatal("unexpected stats type")
	}
	if stats["capacity"] != 10 {
		t.Errorf("expected capacity 10, got %d", stats["capacity"])
	}
}

func TestSchedulerGetEventsLimit(t *testing.T) {
	s := NewScheduler()
	s.Start()
	defer s.Stop()

	// Register several primitives to generate events
	for i := 0; i < 20; i++ {
		s.RegisterPrimitive(
			string(rune('a'+i)),
			TypeMutex,
			"m",
			nil,
		)
	}

	// Give the eventWriter time to process events
	time.Sleep(50 * time.Millisecond)

	all := s.GetEvents(0)
	limited := s.GetEvents(5)
	if len(limited) > 5 {
		t.Errorf("GetEvents(5) should return at most 5, got %d", len(limited))
	}
	if len(all) > len(s.events) {
		t.Errorf("GetEvents(0) should return all events")
	}
}

func TestSchedulerStopIdempotent(t *testing.T) {
	s := NewScheduler()
	s.Start()
	s.Stop()
	// Second Stop must not panic
	s.Stop()
}

func TestSchedulerUptimeNonZero(t *testing.T) {
	s := NewScheduler()
	s.Start()
	defer s.Stop()

	time.Sleep(10 * time.Millisecond)
	m := s.GetMetrics()
	if m.Uptime <= 0 {
		t.Errorf("Uptime should be positive, got %v", m.Uptime)
	}
}

func TestSchedulerGoroutinePruning(t *testing.T) {
	s := NewScheduler()
	s.Start()
	defer s.Stop()

	// Register and immediately unregister; goroutine must be removed.
	s.RegisterGoroutine(42, "worker-42")
	s.UnregisterGoroutine(42)

	goroutines := s.GetGoroutines()
	if _, ok := goroutines[42]; ok {
		t.Error("finished goroutine should be pruned from map immediately")
	}
}

func TestSchedulerMaxGoroutineCap(t *testing.T) {
	s := NewScheduler()
	s.Start()
	defer s.Stop()

	// Register up to the cap; beyond-cap registrations are silently dropped.
	for i := uint64(0); i < uint64(maxGoroutines+10); i++ {
		s.RegisterGoroutine(i, "g")
	}

	goroutines := s.GetGoroutines()
	if len(goroutines) > maxGoroutines {
		t.Errorf("goroutine map exceeded cap: len=%d max=%d", len(goroutines), maxGoroutines)
	}
}

func TestSchedulerBlockUnblock(t *testing.T) {
	s := NewScheduler()
	s.Start()
	defer s.Stop()

	s.RegisterPrimitive("lock1", TypeMutex, "lock", nil)
	s.RegisterGoroutine(1, "g1")

	s.BlockGoroutine(1, "lock1")
	goroutines := s.GetGoroutines()
	if goroutines[1].State != StateBlocked {
		t.Errorf("expected Blocked, got %s", goroutines[1].State)
	}

	s.UnblockGoroutine(1, 5*time.Millisecond)
	goroutines = s.GetGoroutines()
	if goroutines[1].State != StateRunning {
		t.Errorf("expected Running after unblock, got %s", goroutines[1].State)
	}
}

func TestSchedulerEventDecoupling(t *testing.T) {
	// Verifies that emit() never blocks the caller (C3).
	s := NewScheduler()
	s.Start()
	defer s.Stop()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Register many primitives rapidly; if emit() blocks the goroutine
		// would be stuck and the channel would not close in time.
		for i := 0; i < 300; i++ {
			s.RegisterPrimitive(
				string(rune('A'+i%26))+string(rune('0'+i%10)),
				TypeBarrier,
				"b",
				nil,
			)
		}
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Error("emit() appears to block the caller (C3 regression)")
	}
}
