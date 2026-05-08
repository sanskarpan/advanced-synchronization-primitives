# TICKET-017: Persistent Audit Log for Primitive Operations

**Type:** feature
**Priority:** P2
**Estimate:** M (3–4 days)
**Epic:** Observability and Reliability
**Labels:** p2, sprint-3, observability, audit, logging, persistence
**Status:** TODO

## Problem Statement

The server maintains an in-memory ring buffer of the last 1000 scheduler events. These events capture primitive creation, goroutine block/unblock, and primitive deletion. However:
1. Events are lost on server restart.
2. The ring buffer discards events when the 1000-entry limit is reached.
3. There is no way to query past events programmatically or export them.
4. Operations cannot be attributed to a specific WebSocket connection or user.

In production environments, operators need an audit trail for compliance ("who acquired lock X at time T?"), debugging ("why did this primitive get deleted?"), and incident response ("what was the state of all primitives 10 minutes ago?").

## Context

The `Scheduler.events []Event` ring buffer has a max size of 1000. Events are written by `appendEvent()` under `eventsMu`. Each event has: `Timestamp`, `Type`, `GoroutineID`, `PrimitiveID`, `Message`.

`Event.Message` is a human-readable string. There is no structured data (e.g., no `ConnID` field, no operation parameters).

## Goals

1. Add `Config.AuditLogPath` — a file path for the audit log (empty = disabled).
2. Write operations to the audit log in JSON Lines (NDJSON) format.
3. Each audit log entry includes: timestamp, event type, primitive ID, primitive type, connection ID, user (from JWT `sub` claim when available), operation, result (success/error), and duration.
4. Audit log writing is non-blocking (separate goroutine, buffered channel).
5. Audit log survives server restarts (append mode).
6. Audit log file is rotated when it exceeds 100 MB (configurable).

## Non-Goals

- Audit log compression or archiving (handled by external log management tools).
- Querying/searching the audit log via the WebSocket API.
- Encrypting the audit log.
- Real-time streaming of audit events via WebSocket (use the existing `/metrics` endpoint).

## Technical Design

### Audit Log Entry (JSON Lines format)

Each line is a complete JSON object:
```json
{
    "ts": "2026-05-09T10:23:45.123456789Z",
    "event": "primitive_op",
    "conn_id": "a1b2c3d4",
    "user": "alice@example.com",
    "primitive_id": "mutex-1",
    "primitive_type": "Mutex",
    "op": "lock",
    "hold_ms": 1000,
    "result": "success",
    "duration_ns": 45123,
    "error": ""
}
```

Event types: `primitive_created`, `primitive_deleted`, `primitive_op`, `conn_opened`, `conn_closed`.

### Implementation

```go
type AuditEntry struct {
    Timestamp     time.Time `json:"ts"`
    Event         string    `json:"event"`
    ConnID        string    `json:"conn_id"`
    User          string    `json:"user,omitempty"`
    PrimitiveID   string    `json:"primitive_id,omitempty"`
    PrimitiveType string    `json:"primitive_type,omitempty"`
    Op            string    `json:"op,omitempty"`
    HoldMs        int       `json:"hold_ms,omitempty"`
    Result        string    `json:"result,omitempty"`
    DurationNs    int64     `json:"duration_ns,omitempty"`
    Error         string    `json:"error,omitempty"`
}

type AuditLogger struct {
    ch       chan AuditEntry
    file     *os.File
    maxBytes int64
    done     chan struct{}
}
```

The `AuditLogger` runs a goroutine that reads from `ch` and writes JSON Lines to `file`. This decouples the hot path from file I/O.

## Backend Implementation

1. Create `internal/audit/audit.go` with `AuditLogger`, `AuditEntry`.
2. Add `AuditLogPath string` and `AuditLogMaxBytes int64` to `Config`.
3. Initialize `AuditLogger` in `NewServerWithConfig` if `AuditLogPath` is non-empty.
4. Call `auditLogger.Log(entry)` in:
   - `HandleWebSocket` on connection open/close.
   - `handlePrimitiveOp` on operation start and after completion.
   - `createMutex` / `createSemaphore` / etc. on creation.
   - `deletePrimitive` on deletion.
5. Pass `connID` and `user` (from JWT claims, if available) to all log calls.
6. Implement log rotation when file size exceeds `AuditLogMaxBytes`.
7. On server shutdown, flush and close the audit log.

## Frontend Implementation

None.

## Database / State Changes

New file: `Config.AuditLogPath` (e.g., `/var/log/syncprim/audit.jsonl`).

## API Changes

None.

## Infrastructure Requirements

- Write permission to the audit log directory.
- Sufficient disk space (estimate: ~500 bytes per entry × 50 ops/s × 86400 s/day = ~2 GB/day at high load).

## Edge Cases

- `ch` channel full: drop the entry and increment a `droppedAuditEntries` counter. Log at `slog.Warn`.
- File write error: log to `slog.Error`, continue operation without the audit log.
- File rotation race: use `sync.Mutex` around file operations in the writer goroutine.
- Long `primitive_id` or `user` fields: validate against the 256-char limit from TICKET-001.

## Failure Handling

- Audit log unavailable: server continues operating normally. Dropped entries are counted and exposed in `/healthz`.
- File permission denied: log at `slog.Error` on startup, disable audit logging.

## Security Considerations

- Audit log contains user IDs from JWT `sub` claims. The log file should be readable only by the server process and operators (permissions: `0640`).
- Do not write API keys or JWT tokens to the audit log.
- The audit log directory should be on a separate filesystem from the OS to prevent audit log exhaustion from causing OS failures.

## Testing Plan

### Unit Tests

```go
func TestAuditLoggerWritesEntry(t *testing.T) {
    path := t.TempDir() + "/audit.jsonl"
    logger := audit.New(path, 10*1024*1024)
    logger.Start()
    logger.Log(audit.AuditEntry{
        Timestamp:   time.Now(),
        Event:       "primitive_op",
        ConnID:      "test-conn",
        PrimitiveID: "mutex-1",
        Op:          "lock",
        Result:      "success",
    })
    logger.Stop()

    data, _ := os.ReadFile(path)
    var entry audit.AuditEntry
    require.NoError(t, json.Unmarshal(bytes.TrimRight(data, "\n"), &entry))
    assert.Equal(t, "primitive_op", entry.Event)
}

func TestAuditLoggerRotation(t *testing.T) {
    // Write entries until rotation occurs
    // Verify old log is archived, new log created
}
```

### Integration Tests

Run `TestWebSocketLoad` with audit logging enabled. Verify audit log contains entries for all operations.

### E2E Tests

Manual: run server with `-audit-log /tmp/audit.jsonl`. Create primitives via dashboard, operate them. Inspect `/tmp/audit.jsonl` with `jq`.

## Monitoring Requirements

- `syncprim_dropped_audit_entries_total` — counter for dropped entries.
- `/healthz` includes `dropped_audit_entries` field.

## Logging Requirements

```
level=INFO  msg="audit log started" path="/var/log/syncprim/audit.jsonl"
level=WARN  msg="audit log channel full; dropping entry" dropped_total=1
level=ERROR msg="audit log write error; disabling audit logging" err="disk full"
level=INFO  msg="audit log rotated" old="/var/log/syncprim/audit.jsonl.1"
```

## Metrics to Track

- `syncprim_dropped_audit_entries_total` — counter

## Rollback Plan

Set `Config.AuditLogPath` to empty to disable. No server restart required if configurable at runtime (not in this ticket). Server restart required otherwise. No operational impact.

## Acceptance Criteria

- [ ] Audit log written in JSON Lines format
- [ ] Each entry includes `ts`, `event`, `conn_id`, `primitive_id`, `op`, `result`
- [ ] JWT user claim (`sub`) included when JWT auth is enabled
- [ ] Log rotation at configurable max bytes
- [ ] Server continues operating if audit log write fails
- [ ] `TestAuditLoggerWritesEntry` passes
- [ ] `syncprim_dropped_audit_entries_total` metric present

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage ≥70%
- [ ] Audit log file permissions documented
- [ ] CHANGELOG entry written
