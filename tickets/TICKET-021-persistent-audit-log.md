# TICKET-021: Persistent Audit Log Ring Buffer to Disk

**Type:** feature
**Priority:** P2
**Estimate:** M (3 days)
**Epic:** Observability and Reliability
**Labels:** p2, sprint-3, observability, audit, persistence
**Status:** SHIPPED

**Tracking Note:** Implemented on `origin/main`. This ticket is retained as historical planning context; the design notes and checklists below may not match the final shipped implementation verbatim.


## Problem Statement

This ticket is a refinement of TICKET-017, focusing specifically on the ring-buffer-to-disk aspect of audit log persistence. TICKET-017 covers the full audit log feature. This ticket covers the implementation details of the in-memory ring buffer that feeds into the disk writer, and the disk rotation/truncation policy.

## Context

The in-memory event ring buffer in the Scheduler holds 1000 events maximum. When full, old events are silently dropped. The audit log in TICKET-017 adds a disk writer, but if the disk writer is slow (e.g., slow NFS mount), the ring buffer may overflow and drop events before they reach the disk.

This ticket ensures:
1. The in-memory buffer between the audit event producer and the disk writer is appropriately sized.
2. Events that cannot be written to disk are counted and exposed in metrics.
3. The disk file is rotated to prevent unbounded growth.

## Goals

1. Implement a fixed-size ring buffer (pre-allocated `[N]AuditEntry` array) for the audit event channel.
2. Implement log rotation: when the current log file exceeds `Config.AuditLogMaxBytes`, rename to `.1` and start a new file.
3. Implement log truncation: keep at most `Config.AuditLogKeepFiles` rotated files.
4. Expose `syncprim_dropped_audit_events_total` counter.
5. Write a `TestAuditRingBufferDropsGracefully` test.

## Non-Goals

- Compression of rotated log files.
- Centralized log shipping (Splunk, ELK, Datadog).
- Log parsing or querying.

## Technical Design

### Ring Buffer for Audit Channel

Replace the `chan AuditEntry` with a fixed-size buffered channel:

```go
const defaultAuditChannelSize = 4096

type AuditLogger struct {
    ch           chan AuditEntry  // buffered channel as ring buffer
    file         *os.File
    maxBytes     int64
    keepFiles    int
    currentBytes int64
    dropped      atomic.Int64
    mu           sync.Mutex // protects file and currentBytes
    done         chan struct{}
    wg           sync.WaitGroup
}
```

The channel acts as an in-memory queue. If the writer goroutine is slow, entries buffer in the channel. If the channel fills, `Log()` increments the drop counter and discards the entry:

```go
func (al *AuditLogger) Log(entry AuditEntry) {
    select {
    case al.ch <- entry:
    default:
        al.dropped.Add(1)
    }
}
```

### Log Rotation

In the writer goroutine, after writing each entry:
```go
al.currentBytes += int64(n)
if al.currentBytes >= al.maxBytes {
    al.rotate()
}
```

`rotate()`:
1. Close current file.
2. Rename `audit.jsonl` → `audit.jsonl.1`, `audit.jsonl.1` → `audit.jsonl.2`, etc.
3. If `keepFiles` is exceeded, delete the oldest.
4. Open a new `audit.jsonl`.

### Graceful Shutdown

On `Stop()`, signal the writer goroutine and wait for it to drain the channel:
```go
func (al *AuditLogger) Stop() {
    close(al.done)
    al.wg.Wait() // writer goroutine drains ch before exiting
}
```

Writer goroutine drain loop:
```go
func (al *AuditLogger) writer() {
    defer al.wg.Done()
    defer al.flush()
    for {
        select {
        case entry := <-al.ch:
            al.writeEntry(entry)
        case <-al.done:
            // Drain remaining entries
            for {
                select {
                case entry := <-al.ch:
                    al.writeEntry(entry)
                default:
                    return
                }
            }
        }
    }
}
```

## Backend Implementation

1. Create `internal/audit/ring_buffer.go` with ring-buffer-backed `AuditLogger`.
2. Implement log rotation in `internal/audit/rotation.go`.
3. Add `AuditLogMaxBytes int64` and `AuditLogKeepFiles int` to `Config`.
4. Wire up the audit logger in `NewServerWithConfig`.
5. Add `TestAuditRingBufferDropsGracefully`: write entries faster than they can be flushed (mock slow I/O), assert drops are counted.
6. Add `TestAuditLogRotation`: write enough entries to trigger rotation, assert both files exist.

## Frontend Implementation

None.

## Database / State Changes

New log files: `audit.jsonl`, `audit.jsonl.1`, ..., `audit.jsonl.N` where N = `AuditLogKeepFiles - 1`.

## API Changes

New `Config` fields: `AuditLogPath`, `AuditLogMaxBytes`, `AuditLogKeepFiles`.

## Infrastructure Requirements

Disk space: `AuditLogMaxBytes × AuditLogKeepFiles`. Default: 100 MB × 5 files = 500 MB.

## Edge Cases

- `AuditLogMaxBytes = 0`: treat as "no rotation" (file grows unboundedly). Document this.
- `AuditLogKeepFiles = 0`: treat as "keep only current file" (no archiving after rotation).
- Rotation during high write load: use `al.mu` to serialize rotation and writes.
- Disk full during write: `al.writeEntry` returns an error → log at `slog.Error`, increment error counter, continue (do not crash the server).

## Failure Handling

All disk I/O failures are non-fatal. The server continues operating. Failures are logged and counted.

## Security Considerations

Rotated log files contain the same sensitive data (user IDs, primitive operation history) as the active log. Apply the same file permissions (`0640`) to rotated files. Delete rotated files securely if they contain sensitive data.

## Testing Plan

### Unit Tests

```go
func TestAuditRingBufferDropsGracefully(t *testing.T) {
    // Create AuditLogger with a very slow mock writer (adds delay)
    // and a small channel (8 entries)
    // Write 100 entries rapidly
    // Assert dropped count > 0 (some were dropped)
    // Assert no panic or deadlock
}

func TestAuditLogRotation(t *testing.T) {
    path := t.TempDir() + "/audit.jsonl"
    logger := audit.New(path, 100, 3) // 100 bytes max, keep 3 files
    logger.Start()
    // Write enough entries to exceed 100 bytes
    for i := 0; i < 100; i++ {
        logger.Log(audit.AuditEntry{Event: "test", ConnID: "c1"})
    }
    logger.Stop()
    // Assert audit.jsonl exists
    _, err1 := os.Stat(path)
    assert.NoError(t, err1)
    // Assert audit.jsonl.1 exists (rotated file)
    _, err2 := os.Stat(path + ".1")
    assert.NoError(t, err2)
}
```

### Integration Tests

Run the server under load with audit logging enabled. Verify no goroutine leaks and no crashes.

### E2E Tests

Manual: run server with audit log, perform 10,000 operations, verify all operations appear in the log.

## Monitoring Requirements

- `syncprim_dropped_audit_events_total` — counter exposed on `/metrics`.
- `/healthz` includes `dropped_audit_events` field.

## Logging Requirements

```
level=WARN  msg="audit log channel full; dropping event" dropped_total=42
level=INFO  msg="audit log rotated" new_file="audit.jsonl" old_file="audit.jsonl.1"
level=WARN  msg="audit log rotation: deleted oldest file" file="audit.jsonl.4"
level=ERROR msg="audit log write error" err="no space left on device"
```

## Metrics to Track

- `syncprim_dropped_audit_events_total` — counter

## Rollback Plan

Set `Config.AuditLogPath = ""` to disable. No operational impact.

## Acceptance Criteria

- [ ] Audit events written to disk in JSON Lines format
- [ ] Ring buffer drops events gracefully when full (no panic, drop counter incremented)
- [ ] Log rotation occurs when file exceeds `AuditLogMaxBytes`
- [ ] At most `AuditLogKeepFiles` files retained after rotation
- [ ] `syncprim_dropped_audit_events_total` metric present
- [ ] `TestAuditRingBufferDropsGracefully` and `TestAuditLogRotation` pass
- [ ] Graceful drain on shutdown (no events lost during normal shutdown)

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage ≥70%
- [ ] Disk space documented
- [ ] CHANGELOG entry written
