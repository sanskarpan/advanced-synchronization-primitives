# TICKET-020: Configurable Prometheus Histogram Bucket Boundaries

**Type:** improvement
**Priority:** P2
**Estimate:** M (3 days)
**Epic:** Observability and Reliability
**Labels:** p2, sprint-3, observability, metrics, prometheus, configuration
**Status:** TODO

## Problem Statement

The Prometheus histogram for wait duration uses hardcoded bucket boundaries:
```go
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
```

These boundaries are appropriate for mid-latency systems but wrong for:
1. **Low-latency applications**: where p99 lock wait is <10 µs — all observations pile into the first bucket, losing distribution information.
2. **High-latency applications**: where locks are held for seconds — everything falls into the `+Inf` bucket.
3. **Production tuning**: operators want custom bucket widths to match SLO thresholds.

## Context

The buckets are defined in `internal/metrics/metrics.go` as a package-level variable. They are used in `PrimitiveMetrics.HistBuckets` which is set in `NewMetricsCollector.RegisterPrimitive`.

## Goals

1. Add `HistogramBuckets []time.Duration` to `web.Config`.
2. When non-nil/non-empty, use the provided bucket boundaries instead of the defaults.
3. Thread the buckets through `NewServerWithConfig` → `MetricsCollector` → `RegisterPrimitive`.
4. Validate: buckets must be positive, in ascending order, with no duplicates.
5. Expose the configured buckets in `/healthz` for operator verification.

## Non-Goals

- Per-primitive bucket configuration.
- Dynamic bucket reconfiguration without restart.
- Exponential bucket generation helper (future convenience feature).

## Technical Design

Add to `Config`:
```go
// HistogramBuckets defines the upper bound boundaries for wait duration histograms.
// Values are in time.Duration (e.g., 100*time.Microsecond, 1*time.Millisecond).
// Default: [100µs, 500µs, 1ms, 5ms, 10ms, 50ms, 100ms, 500ms, 1s]
// Must be in ascending order with no duplicates.
HistogramBuckets []time.Duration
```

Update `NewMetricsCollectorWithBuckets`:
```go
func NewMetricsCollectorWithBuckets(buckets []time.Duration) *MetricsCollector {
    bounds := make([]int64, len(buckets))
    for i, b := range buckets {
        bounds[i] = b.Nanoseconds()
    }
    return &MetricsCollector{
        startTime:        time.Now(),
        primitiveMetrics: make(map[string]*PrimitiveMetrics),
        histBounds:       bounds,
    }
}
```

Validation in `NewServerWithConfig`:
```go
func validateHistogramBuckets(buckets []time.Duration) error {
    for i := 1; i < len(buckets); i++ {
        if buckets[i] <= buckets[i-1] {
            return fmt.Errorf("histogram buckets must be in strictly ascending order: buckets[%d]=%v <= buckets[%d]=%v",
                i, buckets[i], i-1, buckets[i-1])
        }
        if buckets[i] <= 0 {
            return fmt.Errorf("histogram buckets must be positive: buckets[%d]=%v", i, buckets[i])
        }
    }
    return nil
}
```

## Backend Implementation

1. Add `HistogramBuckets []time.Duration` to `Config`.
2. Move `histBoundaries` from `metrics.go` to a package-level default variable.
3. Add `histBounds []int64` field to `MetricsCollector` (replacing the package-level `histBoundaries`).
4. Update `RegisterPrimitive` to use `mc.histBounds` instead of the package-level variable.
5. Implement `validateHistogramBuckets`.
6. Update `NewServerWithConfig` to validate and pass buckets to `MetricsCollector`.
7. Update `NewMetricsCollector()` (zero-config) to use the default bounds.
8. Expose configured buckets in `/healthz` as `"histogram_buckets_ms": [0.1, 0.5, 1, 5, ...]`.
9. Add tests for validation (ascending order, positive values, empty = defaults).

## Frontend Implementation

None.

## Database / State Changes

None.

## API Changes

New optional field in `Config`. Zero value (nil slice) uses the current hardcoded defaults. Backward compatible.

## Infrastructure Requirements

None.

## Edge Cases

- Empty slice `[]time.Duration{}`: use defaults. Do not treat as "no histogram."
- Single bucket: valid. All observations fall either into the one bucket or `+Inf`.
- Very large bucket (e.g., `24*time.Hour`): valid. The `+Inf` bucket will be empty in most cases.
- Zero value bucket (e.g., `0`): reject with validation error.

## Failure Handling

- Invalid bucket configuration: `NewServerWithConfig` returns an error (change its signature to return `(*Server, error)`). Currently it returns only `*Server`. This is a breaking change to the constructor signature. Alternatively, panic on invalid configuration.

Decision: panic on invalid histogram configuration. Invalid bucket boundaries are a programming error (set at startup), not a runtime error. Consistent with Go standard library behavior (e.g., `http.NewServeMux()` panics on bad patterns in Go 1.22).

## Security Considerations

None.

## Testing Plan

### Unit Tests

```go
func TestCustomHistogramBuckets(t *testing.T) {
    buckets := []time.Duration{
        10 * time.Microsecond,
        100 * time.Microsecond,
        1 * time.Millisecond,
        10 * time.Millisecond,
    }
    srv := web.NewServerWithConfig(web.Config{
        HistogramBuckets: buckets,
        AllowedOrigins:   []string{"*"},
    })
    // Connect, create mutex, lock it
    // Record a wait that falls into the 100µs-1ms bucket
    // Check /metrics output has the custom bucket boundaries
}

func TestDefaultHistogramBucketsWhenEmpty(t *testing.T) {
    srv := web.NewServerWithConfig(web.Config{})
    // Verify default buckets are used
    // Assert /metrics output has the 9 default buckets
}

func TestInvalidHistogramBucketsPanics(t *testing.T) {
    defer func() {
        if r := recover(); r == nil {
            t.Fatal("expected panic for non-ascending buckets")
        }
    }()
    web.NewServerWithConfig(web.Config{
        HistogramBuckets: []time.Duration{
            1 * time.Millisecond,
            100 * time.Microsecond, // invalid: not ascending
        },
    })
}
```

### Integration Tests

Existing metrics tests. Add a test that verifies bucket boundaries appear correctly in Prometheus text format.

### E2E Tests

Manual: start server with custom histogram buckets via environment variable (once TICKET-004 env vars are implemented). Verify `/metrics` shows the custom buckets.

## Monitoring Requirements

Expose configured bucket boundaries in `/healthz`:
```json
{
    "status": "ok",
    "uptime": "1h23m",
    "histogram_buckets": ["100µs", "500µs", "1ms", "5ms", "10ms", "50ms", "100ms", "500ms", "1s"]
}
```

## Logging Requirements

Log the configured histogram bucket boundaries at startup:
```
level=INFO msg="histogram buckets configured" buckets=[100µs 500µs 1ms 5ms 10ms 50ms 100ms 500ms 1s]
```

## Metrics to Track

The configured buckets define the histogram boundaries. No new counters.

## Rollback Plan

Remove `HistogramBuckets` from `Config`. The default hardcoded boundaries are always used. No data loss.

## Acceptance Criteria

- [ ] Custom bucket boundaries respected in histogram output
- [ ] Default boundaries used when `HistogramBuckets` is nil
- [ ] Non-ascending buckets panic with an informative message
- [ ] Bucket boundaries appear in `/healthz` response
- [ ] All existing metrics tests pass

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage ≥70%
- [ ] README configuration table updated
- [ ] CHANGELOG entry written
