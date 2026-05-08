package metrics

import (
	"sync"
	"testing"
	"time"
)

func TestMetricsRecordAcquireRelease(t *testing.T) {
	mc := NewMetricsCollector()
	mc.RegisterPrimitive("sem1", "Semaphore", "s1")

	mc.RecordAcquire("sem1", 3)
	mc.RecordRelease("sem1", 4)

	snap := mc.GetPrimitiveMetrics("sem1")
	if snap == nil {
		t.Fatal("expected metrics snapshot, got nil")
	}
	if snap.Acquires != 1 {
		t.Errorf("expected 1 acquire, got %d", snap.Acquires)
	}
	if snap.Releases != 1 {
		t.Errorf("expected 1 release, got %d", snap.Releases)
	}
	if snap.CurrentHolders != 4 {
		t.Errorf("expected CurrentHolders 4, got %d", snap.CurrentHolders)
	}
}

func TestMetricsRecordWaitMinMax(t *testing.T) {
	mc := NewMetricsCollector()
	mc.RegisterPrimitive("m1", "Mutex", "mu")

	mc.RecordWait("m1", 10*time.Millisecond, 1)
	mc.RecordWait("m1", 50*time.Millisecond, 2)
	mc.RecordWait("m1", 5*time.Millisecond, 1)

	snap := mc.GetPrimitiveMetrics("m1")
	if snap == nil {
		t.Fatal("expected metrics snapshot, got nil")
	}
	if snap.Waits != 3 {
		t.Errorf("expected 3 waits, got %d", snap.Waits)
	}
	if snap.MinWaitTime > 10*time.Millisecond {
		t.Errorf("MinWaitTime too large: %v", snap.MinWaitTime)
	}
	if snap.MaxWaitTime < 50*time.Millisecond {
		t.Errorf("MaxWaitTime too small: %v", snap.MaxWaitTime)
	}
}

func TestMetricsConcurrentRace(t *testing.T) {
	mc := NewMetricsCollector()
	mc.RegisterPrimitive("p1", "RWLock", "rw")

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			mc.RecordAcquire("p1", int32(n))
			mc.RecordWait("p1", time.Duration(n)*time.Millisecond, int32(n))
			mc.RecordRelease("p1", int32(n))
			mc.RecordTimeout("p1")
		}(i)
	}
	wg.Wait()

	snap := mc.GetPrimitiveMetrics("p1")
	if snap == nil {
		t.Fatal("expected metrics snapshot")
	}
	if snap.Acquires != 20 {
		t.Errorf("expected 20 acquires, got %d", snap.Acquires)
	}
	if snap.Releases != 20 {
		t.Errorf("expected 20 releases, got %d", snap.Releases)
	}
	if snap.Timeouts != 20 {
		t.Errorf("expected 20 timeouts, got %d", snap.Timeouts)
	}
}

func TestMetricsGlobalMetricsNoDeadlockFields(t *testing.T) {
	mc := NewMetricsCollector()
	mc.RegisterPrimitive("x", "Barrier", "b")
	mc.RecordAcquire("x", 1)

	g := mc.GetGlobalMetrics()
	if g.TotalAcquires != 1 {
		t.Errorf("expected 1 total acquire, got %d", g.TotalAcquires)
	}
	// Uptime should be non-zero
	if g.Uptime <= 0 {
		t.Errorf("expected positive uptime, got %v", g.Uptime)
	}
}

// TestMetricsHistogram verifies that histogram buckets are incremented correctly.
func TestMetricsHistogram(t *testing.T) {
	mc := NewMetricsCollector()
	mc.RegisterPrimitive("histo", "Mutex", "h")

	// 50µs — should land in the first bucket (<=100µs = 100_000 ns)
	mc.RecordWait("histo", 50*time.Microsecond, 0)
	// 200µs — should land in the second bucket (<=500µs = 500_000 ns)
	mc.RecordWait("histo", 200*time.Microsecond, 0)
	// 2s — above the last bucket (1s), goes into Inf
	mc.RecordWait("histo", 2*time.Second, 0)

	hist := mc.GetHistogram("histo")
	if hist == nil {
		t.Fatal("expected histogram, got nil")
	}

	// First bucket (<=0.0001s) should have count 1.
	if hist[0].Count != 1 {
		t.Errorf("bucket[0] (<=100µs): expected 1, got %d", hist[0].Count)
	}
	// Second bucket (<=0.0005s) should have count 1.
	if hist[1].Count != 1 {
		t.Errorf("bucket[1] (<=500µs): expected 1, got %d", hist[1].Count)
	}
	// +Inf bucket should have count 1.
	last := hist[len(hist)-1]
	if last.Count != 1 {
		t.Errorf("hist[+Inf]: expected 1, got %d", last.Count)
	}

	// Also verify via snapshot.
	snap := mc.GetPrimitiveMetrics("histo")
	if snap == nil {
		t.Fatal("expected snapshot")
	}
	if len(snap.Histogram) == 0 {
		t.Error("expected non-empty histogram in snapshot")
	}
}

func TestMetricsUnregister(t *testing.T) {
	mc := NewMetricsCollector()
	mc.RegisterPrimitive("del1", "Mutex", "m")
	mc.UnregisterPrimitive("del1")

	if snap := mc.GetPrimitiveMetrics("del1"); snap != nil {
		t.Error("expected nil after unregister")
	}
}

// TestMetricsGetAllMetrics verifies that GetAllMetrics returns all registered
// primitives and their snapshots.
func TestMetricsGetAllMetrics(t *testing.T) {
	mc := NewMetricsCollector()
	mc.RegisterPrimitive("p1", "Mutex", "m1")
	mc.RegisterPrimitive("p2", "Semaphore", "s1")

	mc.RecordAcquire("p1", 0)
	mc.RecordWait("p1", 5*time.Millisecond, 0)

	all := mc.GetAllMetrics()
	if len(all) < 2 {
		t.Errorf("expected at least 2 entries, got %d", len(all))
	}
	if _, ok := all["p1"]; !ok {
		t.Error("p1 missing from GetAllMetrics")
	}
	if _, ok := all["p2"]; !ok {
		t.Error("p2 missing from GetAllMetrics")
	}
}
