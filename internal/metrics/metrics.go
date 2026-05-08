package metrics

import (
	"math"
	"sync"
	"sync/atomic"
	"time"
)

// histBoundaries defines histogram bucket upper bounds in nanoseconds.
// Corresponds to: 100µs, 500µs, 1ms, 5ms, 10ms, 50ms, 100ms, 500ms, 1s.
var histBoundaries = []int64{
	100_000,       // 100µs
	500_000,       // 500µs
	1_000_000,     // 1ms
	5_000_000,     // 5ms
	10_000_000,    // 10ms
	50_000_000,    // 50ms
	100_000_000,   // 100ms
	500_000_000,   // 500ms
	1_000_000_000, // 1s
}

// HistogramBucket represents a single histogram bucket.
type HistogramBucket struct {
	Le    float64 // upper bound in seconds (use +Inf for the overflow bucket)
	Count int64
}

// MetricsCollector collects and aggregates metrics
type MetricsCollector struct {
	// Overall metrics
	startTime time.Time

	// Lock contention metrics
	lockContentions atomic.Int64

	// Per-primitive metrics
	primitiveMetrics map[string]*PrimitiveMetrics
	mu               sync.RWMutex
}

// PrimitiveMetrics contains metrics for a specific primitive
type PrimitiveMetrics struct {
	Type string
	Name string

	// Operation counts
	Acquires atomic.Int64
	Releases atomic.Int64
	Waits    atomic.Int64
	Timeouts atomic.Int64

	// Timing
	TotalWaitTime atomic.Int64 // nanoseconds
	MinWaitTime   atomic.Int64 // nanoseconds
	MaxWaitTime   atomic.Int64 // nanoseconds

	// Histogram of wait durations.
	// HistBuckets is set once at creation (immutable slice of bounds in ns).
	// HistCounts[i] counts observations <= HistBuckets[i].
	// HistInf counts observations above the last bucket.
	HistBuckets []int64
	HistCounts  []atomic.Int64
	HistInf     atomic.Int64

	// Current state
	CurrentHolders atomic.Int32
	CurrentWaiters atomic.Int32

	// Contention
	ContentionEvents  atomic.Int64
	MaxContentionSeen atomic.Int32

	CreatedAt time.Time
}

// NewMetricsCollector creates a new metrics collector
func NewMetricsCollector() *MetricsCollector {
	return &MetricsCollector{
		startTime:        time.Now(),
		primitiveMetrics: make(map[string]*PrimitiveMetrics),
	}
}

// RegisterPrimitive registers a new primitive for metrics tracking
func (mc *MetricsCollector) RegisterPrimitive(id, ptype, name string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	pm := &PrimitiveMetrics{
		Type:        ptype,
		Name:        name,
		CreatedAt:   time.Now(),
		HistBuckets: histBoundaries,
		HistCounts:  make([]atomic.Int64, len(histBoundaries)),
	}
	mc.primitiveMetrics[id] = pm
}

// UnregisterPrimitive removes a primitive from metrics tracking
func (mc *MetricsCollector) UnregisterPrimitive(id string) {
	mc.mu.Lock()
	defer mc.mu.Unlock()

	delete(mc.primitiveMetrics, id)
}

// RecordAcquire records an acquire operation
func (mc *MetricsCollector) RecordAcquire(id string, holders int32) {
	mc.mu.RLock()
	metrics, exists := mc.primitiveMetrics[id]
	mc.mu.RUnlock()

	if exists {
		metrics.Acquires.Add(1)
		metrics.CurrentHolders.Store(holders)
	}
}

// RecordRelease records a release operation
func (mc *MetricsCollector) RecordRelease(id string, holders int32) {
	mc.mu.RLock()
	metrics, exists := mc.primitiveMetrics[id]
	mc.mu.RUnlock()

	if exists {
		metrics.Releases.Add(1)
		metrics.CurrentHolders.Store(holders)
	}
}

// RecordWait records a wait operation
func (mc *MetricsCollector) RecordWait(id string, waitTime time.Duration, waiters int32) {
	mc.mu.RLock()
	metrics, exists := mc.primitiveMetrics[id]
	mc.mu.RUnlock()

	if !exists {
		return
	}

	waitNs := waitTime.Nanoseconds()

	metrics.Waits.Add(1)
	metrics.TotalWaitTime.Add(waitNs)
	metrics.CurrentWaiters.Store(waiters)

	// Increment histogram bucket.
	recorded := false
	for i, bound := range metrics.HistBuckets {
		if waitNs <= bound {
			metrics.HistCounts[i].Add(1)
			recorded = true
			break
		}
	}
	if !recorded {
		metrics.HistInf.Add(1)
	}

	// Update min/max wait times
	for {
		current := metrics.MinWaitTime.Load()
		if current == 0 || waitNs < current {
			if metrics.MinWaitTime.CompareAndSwap(current, waitNs) {
				break
			}
		} else {
			break
		}
	}

	for {
		current := metrics.MaxWaitTime.Load()
		if waitNs > current {
			if metrics.MaxWaitTime.CompareAndSwap(current, waitNs) {
				break
			}
		} else {
			break
		}
	}

	// Record contention if there were multiple waiters
	if waiters > 1 {
		mc.lockContentions.Add(1)
		metrics.ContentionEvents.Add(1)

		// Update max contention seen
		for {
			current := metrics.MaxContentionSeen.Load()
			if waiters > current {
				if metrics.MaxContentionSeen.CompareAndSwap(current, waiters) {
					break
				}
			} else {
				break
			}
		}
	}
}

// RecordTimeout records a timeout event
func (mc *MetricsCollector) RecordTimeout(id string) {
	mc.mu.RLock()
	metrics, exists := mc.primitiveMetrics[id]
	mc.mu.RUnlock()

	if exists {
		metrics.Timeouts.Add(1)
	}
}

// GetHistogram returns the wait-duration histogram for a specific primitive.
// Returns nil if the primitive is not registered.
func (mc *MetricsCollector) GetHistogram(id string) []HistogramBucket {
	mc.mu.RLock()
	pm, exists := mc.primitiveMetrics[id]
	mc.mu.RUnlock()
	if !exists {
		return nil
	}
	result := make([]HistogramBucket, 0, len(pm.HistBuckets)+1)
	for i, bound := range pm.HistBuckets {
		result = append(result, HistogramBucket{
			Le:    float64(bound) / 1e9, // nanoseconds to seconds
			Count: pm.HistCounts[i].Load(),
		})
	}
	result = append(result, HistogramBucket{
		Le:    math.Inf(1),
		Count: pm.HistInf.Load(),
	})
	return result
}

// GetPrimitiveMetrics returns metrics for a specific primitive
func (mc *MetricsCollector) GetPrimitiveMetrics(id string) *PrimitiveMetricsSnapshot {
	mc.mu.RLock()
	metrics, exists := mc.primitiveMetrics[id]
	mc.mu.RUnlock()

	if !exists {
		return nil
	}

	// Build histogram snapshot.
	hist := make([]HistogramBucket, 0, len(metrics.HistBuckets)+1)
	for i, bound := range metrics.HistBuckets {
		hist = append(hist, HistogramBucket{
			Le:    float64(bound) / 1e9,
			Count: metrics.HistCounts[i].Load(),
		})
	}
	hist = append(hist, HistogramBucket{
		Le:    math.Inf(1),
		Count: metrics.HistInf.Load(),
	})

	return &PrimitiveMetricsSnapshot{
		Type:              metrics.Type,
		Name:              metrics.Name,
		Acquires:          metrics.Acquires.Load(),
		Releases:          metrics.Releases.Load(),
		Waits:             metrics.Waits.Load(),
		Timeouts:          metrics.Timeouts.Load(),
		TotalWaitTime:     time.Duration(metrics.TotalWaitTime.Load()),
		MinWaitTime:       time.Duration(metrics.MinWaitTime.Load()),
		MaxWaitTime:       time.Duration(metrics.MaxWaitTime.Load()),
		AvgWaitTime:       mc.calculateAvgWaitTime(metrics),
		CurrentHolders:    metrics.CurrentHolders.Load(),
		CurrentWaiters:    metrics.CurrentWaiters.Load(),
		ContentionEvents:  metrics.ContentionEvents.Load(),
		MaxContentionSeen: metrics.MaxContentionSeen.Load(),
		Age:               time.Since(metrics.CreatedAt),
		Histogram:         hist,
	}
}

// GetAllMetrics returns metrics for all primitives
func (mc *MetricsCollector) GetAllMetrics() map[string]*PrimitiveMetricsSnapshot {
	mc.mu.RLock()
	ids := make([]string, 0, len(mc.primitiveMetrics))
	for id := range mc.primitiveMetrics {
		ids = append(ids, id)
	}
	mc.mu.RUnlock()

	result := make(map[string]*PrimitiveMetricsSnapshot)
	for _, id := range ids {
		if snapshot := mc.GetPrimitiveMetrics(id); snapshot != nil {
			result[id] = snapshot
		}
	}

	return result
}

// GetGlobalMetrics returns overall metrics
func (mc *MetricsCollector) GetGlobalMetrics() GlobalMetrics {
	mc.mu.RLock()
	totalPrimitives := len(mc.primitiveMetrics)

	var totalAcquires, totalReleases, totalWaits, totalTimeouts int64
	var totalWaitTime int64
	var totalContentions int64

	for _, metrics := range mc.primitiveMetrics {
		totalAcquires += metrics.Acquires.Load()
		totalReleases += metrics.Releases.Load()
		totalWaits += metrics.Waits.Load()
		totalTimeouts += metrics.Timeouts.Load()
		totalWaitTime += metrics.TotalWaitTime.Load()
		totalContentions += metrics.ContentionEvents.Load()
	}
	mc.mu.RUnlock()

	avgWaitTime := time.Duration(0)
	if totalWaits > 0 {
		avgWaitTime = time.Duration(totalWaitTime / totalWaits)
	}

	return GlobalMetrics{
		TotalPrimitives:  totalPrimitives,
		TotalAcquires:    totalAcquires,
		TotalReleases:    totalReleases,
		TotalWaits:       totalWaits,
		TotalTimeouts:    totalTimeouts,
		TotalContentions: totalContentions,
		AvgWaitTime:      avgWaitTime,
		Uptime:           time.Since(mc.startTime),
	}
}

// calculateAvgWaitTime calculates average wait time for a primitive
func (mc *MetricsCollector) calculateAvgWaitTime(metrics *PrimitiveMetrics) time.Duration {
	waits := metrics.Waits.Load()
	if waits == 0 {
		return 0
	}

	totalWait := metrics.TotalWaitTime.Load()
	return time.Duration(totalWait / waits)
}

// PrimitiveMetricsSnapshot is a snapshot of metrics for a primitive
type PrimitiveMetricsSnapshot struct {
	Type              string
	Name              string
	Acquires          int64
	Releases          int64
	Waits             int64
	Timeouts          int64
	TotalWaitTime     time.Duration
	MinWaitTime       time.Duration
	MaxWaitTime       time.Duration
	AvgWaitTime       time.Duration
	CurrentHolders    int32
	CurrentWaiters    int32
	ContentionEvents  int64
	MaxContentionSeen int32
	Age               time.Duration
	Histogram         []HistogramBucket
}

// GlobalMetrics contains overall system metrics.
// Deadlock detection metrics are intentionally excluded — they were always
// zero and misleading. Use external tooling (e.g. go tool trace) for
// deadlock analysis.
type GlobalMetrics struct {
	TotalPrimitives  int
	TotalAcquires    int64
	TotalReleases    int64
	TotalWaits       int64
	TotalTimeouts    int64
	TotalContentions int64
	AvgWaitTime      time.Duration
	Uptime           time.Duration
}
