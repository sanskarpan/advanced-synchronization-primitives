# Performance Benchmarking Checklist

**Project:** Advanced Synchronization Primitives
**Version:** v0.2.0
**Updated:** 2026-05-09

Run this checklist before releases that touch hot paths (primitives, scheduler, server message dispatch). Compare results against the previous release baseline using `benchstat`.

---

## Environment Setup

Before running any benchmarks:

```bash
# Disable CPU frequency scaling (Linux)
sudo cpupower frequency-set -g performance

# Disable garbage collection to isolate allocation benchmarks
GOGC=off go test -bench=. -benchmem ./internal/primitives/

# Standard benchmark run (GC enabled, as in production)
go test -bench=. -benchmem -count=10 ./internal/primitives/ > current.txt
```

Use a dedicated machine or at minimum close all other applications before benchmarking.

---

## 1. Primitive Benchmarks

### Mutex

- [ ] `BenchmarkMutexUncontended` — target: < 30 ns/op, 0 allocs/op
  ```bash
  go test -bench=BenchmarkMutexUncontended -benchmem -count=10 ./internal/primitives/
  ```

- [ ] `BenchmarkMutexContended` (8 goroutines) — target: < 200 ns/op
  ```bash
  go test -bench=BenchmarkMutexContended -benchmem -count=10 ./internal/primitives/
  ```

- [ ] Compare with `sync.Mutex` baseline:
  ```bash
  go test -bench=BenchmarkStdMutex -benchmem -count=10 ./internal/primitives/
  ```
  Acceptable: our implementation within 2× of stdlib.

### RWLock

- [ ] `BenchmarkRWLockRead` (no contention) — target: < 35 ns/op
- [ ] `BenchmarkRWLockReadHeavy` (7 readers, 1 writer) — target: < 120 ns/op
- [ ] Reader scalability: run with `GOMAXPROCS=1`, `GOMAXPROCS=4`, `GOMAXPROCS=8`; verify throughput scales with readers

### Semaphore

- [ ] `BenchmarkSemaphore` (uncontended) — target: < 50 ns/op
- [ ] `BenchmarkSemaphoreBurst` (10 goroutines, capacity 5) — target: < 250 ns/op
- [ ] `BenchmarkSemaphoreAcquireRelease` — target: < 60 ns/op

### Barrier

- [ ] `BenchmarkBarrierSmall` (2 parties) — target: < 400 ns/op
- [ ] `BenchmarkBarrierLarge` (16 parties) — target: < 1.5 µs/op
- [ ] Barrier generation throughput: trips/second at various party counts

### WaitGroup

- [ ] `BenchmarkWaitGroupAddDone` — target: < 25 ns/op
- [ ] `BenchmarkWaitGroupContended` (multiple goroutines calling Done simultaneously)

### Once

- [ ] `BenchmarkOnce` (after done, fast path) — target: < 5 ns/op (atomic load)
- [ ] `BenchmarkOnceContended` (concurrent Do calls) — target: < 50 ns/op

### Singleflight

- [ ] `BenchmarkSingleflightNoContention` — target: < 200 ns/op
- [ ] `BenchmarkSingleflightHighContention` (100 goroutines, same key)

---

## 2. WaiterQueue Benchmarks

- [ ] `BenchmarkWaiterQueueEnqueue` — target: < 50 ns/op
- [ ] `BenchmarkWaiterQueueDequeue` — target: < 30 ns/op
- [ ] `BenchmarkWaiterQueueConcurrent` (MPMC, 4 producers, 4 consumers)

---

## 3. WebSocket Server Benchmarks

- [ ] `BenchmarkWebSocketConcurrentOps` (8 workers, one conn per worker) — target: > 500 create/lock/delete cycles/second per worker
  ```bash
  go test -bench=BenchmarkWebSocketConcurrentOps -benchmem -count=5 -timeout 120s ./internal/loadtest/
  ```

---

## 4. Memory Allocation Profiling

Run the heap profiler during a load test:

```bash
# Start server with pprof enabled
go run ./cmd/server/... &

# Run load test for 30 seconds
go test -v -run=TestWebSocketLoad -timeout 60s ./internal/loadtest/

# Capture heap profile
curl http://localhost:8085/debug/pprof/heap > heap.out
go tool pprof -top heap.out
```

- [ ] Top allocator is NOT `primitives.NewWaiter` (after TICKET-025, pool is in place)
- [ ] Top allocator is NOT `json.Marshal` for update messages (after TICKET-018, delta updates reduce this)
- [ ] Heap size is stable over 5 minutes of load (no memory leak)
- [ ] Goroutine count is stable over 5 minutes of load (no goroutine leak)

---

## 5. CPU Profiling

```bash
go test -bench=BenchmarkMutexContended -cpuprofile cpu.out -count=5 ./internal/primitives/
go tool pprof -top cpu.out
```

- [ ] More than 50% of CPU time is in the primitive's hot path (not in scheduler, logging, or JSON serialization)
- [ ] `runtime.gcBgMarkWorker` is < 10% of CPU time (healthy GC overhead)

---

## 6. Scheduler Benchmarks

- [ ] `BenchmarkSchedulerRegisterPrimitive` — target: < 500 ns/op (involves mutex)
- [ ] `BenchmarkSchedulerGetPrimitives` (100 primitives) — target: < 5 µs/op
- [ ] `BenchmarkSchedulerBroadcast` (100 primitives, 100 ms tick) — verify no CPU spike

---

## 7. Metrics Benchmarks

- [ ] `BenchmarkMetricsRecordAcquire` — target: < 100 ns/op (involves RWMutex read)
- [ ] `BenchmarkMetricsGetAllMetrics` (100 primitives) — target: < 10 µs/op
- [ ] `BenchmarkMetricsPrometheusFormat` — target: < 1 ms for 100 primitives

---

## 8. Comparison Against Previous Release

```bash
# Get baseline from main branch
git checkout main
go test -bench=. -benchmem -count=10 ./internal/primitives/ > baseline.txt

# Switch to release candidate
git checkout release/v0.2.0
go test -bench=. -benchmem -count=10 ./internal/primitives/ > candidate.txt

# Compare
benchstat baseline.txt candidate.txt
```

- [ ] No benchmark regresses by more than 20% in `ns/op`
- [ ] `allocs/op` for Mutex hot path is 0 (or documented and justified)
- [ ] Memory (`B/op`) is not increasing for hot paths

---

## 9. Scalability Tests

### Connection Scaling

Measure throughput at: 1, 10, 50, 100, 500, 1000 concurrent connections.

```bash
# Use the load test with varying connection counts
# Modify TestWebSocketLoad or write a script
```

- [ ] Throughput scales linearly with connections up to CPU saturation
- [ ] Latency remains < 1 ms p99 for Mutex operations at 100 connections
- [ ] Memory per connection ≤ 1 MB at idle

### Primitive Count Scaling

Measure broadcast message size and serialization time at: 1, 10, 50, 100, 500 primitives.

- [ ] Broadcast message size is linear with primitive count
- [ ] After TICKET-018 (delta updates): broadcast message size does not grow with idle primitives

---

## 10. Regression Test Suite

Run the full benchmark suite as part of release validation:

```bash
make bench 2>&1 | tee bench_results.txt
```

- [ ] All benchmarks complete without errors
- [ ] No benchmark takes > 60 seconds (indicates a hang or deadlock)
- [ ] Results saved as `bench_results_v{version}.txt` and committed to the repo

---

## Sign-off

| Section | Tester | Date | Status | Notes |
|---------|--------|------|--------|-------|
| Mutex benchmarks | | | | |
| RWLock benchmarks | | | | |
| Semaphore benchmarks | | | | |
| Barrier benchmarks | | | | |
| WaitGroup benchmarks | | | | |
| WebSocket benchmarks | | | | |
| Memory profiling | | | | |
| CPU profiling | | | | |
| Release comparison | | | | |
| Scalability tests | | | | |

**Performance Status:** [ ] PASS  [ ] FAIL (regressions listed below)

**Regressions Found:**
(List any benchmarks that regressed >20%, with before/after values and proposed fix)
