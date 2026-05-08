# TICKET-001: Input Length Validation for ID, Name, and String Fields

**Type:** security
**Priority:** P0
**Estimate:** S (1–2 days)
**Epic:** Security and Stability Hardening
**Labels:** security, p0, sprint-1, web-server
**Status:** TODO

## Problem Statement

The WebSocket message handler in `web/server.go` deserializes JSON messages and immediately dispatches them without validating string field lengths. A malicious or buggy client can send an `id` or `name` field of arbitrary length (up to the 64 KiB message size cap). These strings are stored as map keys in the scheduler's `primitives` map, logged via `slog`, serialized into every subsequent broadcast message to all connected clients, and potentially written to the snapshot file.

With 1000 connections each sending 200 msg/s (the current rate limit), a single rogue connection can force the server to store and broadcast up to 64 KiB of junk per message, creating a denial-of-service amplification vector.

## Context

- Current message size cap: 64 KiB per WebSocket frame (set via `conn.SetReadLimit(64 * 1024)`)
- Current rate limit: 200 messages/second per connection
- No validation of `id`, `name`, `op`, or other string fields beyond JSON deserialization
- The scheduler stores primitive IDs as map keys: `s.primitives[id]`
- The scheduler broadcasts all primitive IDs to all clients in `SchedulerUpdate.Primitives`
- Log lines in `slog` include `id` and `name` fields; an oversized string bloats log output

Affected message types:
- `createRWLock`: `id`, `name`
- `createSemaphore`: `id`, `name`
- `createMutex`: `id`, `name`
- `createCondVar`: `id`, `name`
- `createBarrier`: `id`, `name`
- `createWaitGroup`: `id`, `name`
- `createOnce`: `id`, `name`
- `createSingleflight`: `id`, `name`
- `primitiveOp`: `id`, `op`
- `deletePrimitive`: `id`

## Goals

1. Reject any inbound WebSocket message where `id` exceeds 256 bytes.
2. Reject any inbound WebSocket message where `name` exceeds 256 bytes.
3. Reject any inbound WebSocket message where `op` exceeds 64 bytes.
4. Return a structured error response instead of silently truncating or discarding.
5. Add a Prometheus counter `syncprim_validation_errors_total{reason="id_too_long"}` for monitoring rejection rate.

## Non-Goals

- Validating semantic correctness of `id` format (e.g., slug format, UUID). Allowed characters are unrestricted within the length limit.
- Modifying the 64 KiB per-message size cap (separate concern).
- Validating `capacity` and `parties` numeric fields (covered by existing type-level validation in the primitives layer; however integer range validation is covered in TICKET-002).

## Technical Design

Add a `validateInboundMessage(msg *inboundMsg) error` function in `web/server.go` called immediately after `json.Unmarshal` in the message read loop.

```go
const (
    maxIDLength   = 256
    maxNameLength = 256
    maxOpLength   = 64
)

func validateInboundMessage(msg *inboundMsg) error {
    if len(msg.Payload.ID) > maxIDLength {
        return fmt.Errorf("id exceeds maximum length of %d characters (got %d)",
            maxIDLength, len(msg.Payload.ID))
    }
    if len(msg.Payload.Name) > maxNameLength {
        return fmt.Errorf("name exceeds maximum length of %d characters (got %d)",
            maxNameLength, len(msg.Payload.Name))
    }
    if len(msg.Payload.Op) > maxOpLength {
        return fmt.Errorf("op exceeds maximum length of %d characters (got %d)",
            maxOpLength, len(msg.Payload.Op))
    }
    return nil
}
```

The validation counter is a new `atomic.Int64` on `*Server` exposed in `HandleMetrics`:
```
syncprim_validation_errors_total{reason="id_too_long"} 0
syncprim_validation_errors_total{reason="name_too_long"} 0
syncprim_validation_errors_total{reason="op_too_long"} 0
```

## Backend Implementation

1. Define constants `maxIDLength = 256`, `maxNameLength = 256`, `maxOpLength = 64` at the top of `web/server.go`.
2. Implement `validateInboundMessage` as described above.
3. Call `validateInboundMessage` at the top of the message dispatch switch, after `json.Unmarshal` succeeds.
4. On validation error: send `{"type":"error","payload":{"message":"<error string>"}}`, increment the validation error counter, and `continue` (do not close the connection — the client may have sent one bad message).
5. Add three `atomic.Int64` counters to `*Server`: `validErrIDTooLong`, `validErrNameTooLong`, `validErrOpTooLong`.
6. Expose these counters in `HandleMetrics`.

## Frontend Implementation

The frontend's inline validation already checks that `capacity` and `parties` are positive integers before sending. Extend the validation to also check `id` and `name` field lengths:

```javascript
if (id.length > 256) {
    showError('Primitive ID must be 256 characters or fewer');
    return;
}
if (name.length > 256) {
    showError('Primitive name must be 256 characters or fewer');
    return;
}
```

This is defense-in-depth; the server validates independently.

## Database / State Changes

None. The validation rejects messages before any state is modified.

## API Changes

No change to the message protocol. Invalid messages receive an `{"type":"error"}` response, which is already the standard error path.

## Infrastructure Requirements

None beyond the existing CI pipeline.

## Edge Cases

- Empty `id` (`""`): allowed by this ticket; a separate ticket can address requiring non-empty IDs.
- Unicode: use `len()` (byte count) not `utf8.RuneCountInString()`. 256 bytes is the limit regardless of character encoding. A 256-byte string with multi-byte Unicode characters may have fewer than 256 visible characters, which is acceptable.
- `id` field not present in JSON payload: `msg.Payload.ID` will be an empty string `""`. This passes length validation. Type-specific handlers may reject it later (e.g., looking up a non-existent primitive).

## Failure Handling

- Validation error: send error response, increment counter, continue reading.
- Counter overflow: `atomic.Int64` can hold 2^63 - 1 values; no overflow risk.

## Security Considerations

- This validation is the primary defense against memory exhaustion via oversized strings.
- The 256-byte limit is generous enough for all legitimate use cases (UUIDs are 36 bytes, human-readable names rarely exceed 64 bytes).
- Do not log the invalid string content at `slog.Error` level — logging the full oversized string in an error message would defeat the protection. Log only the length: `slog.Warn("validation error", "reason", "id_too_long", "len", len(msg.Payload.ID))`.

## Testing Plan

### Unit Tests

In `web/server_test.go`:

```go
func TestValidationIDTooLong(t *testing.T) {
    // Connect via WebSocket, send createMutex with id of length 257
    // Assert response type is "error"
    // Assert response payload.message contains "id exceeds maximum length"
}

func TestValidationNameTooLong(t *testing.T) {
    // Similar test for name exceeding 256 bytes
}

func TestValidationExactlyAtLimit(t *testing.T) {
    // Send createMutex with id of exactly 256 bytes
    // Assert response type is "success" (boundary condition)
}

func TestValidationEmptyID(t *testing.T) {
    // Send createMutex with empty id
    // Assert behavior is unchanged (error from handler, not validation)
}
```

### Integration Tests

Run the load test (`TestWebSocketLoad`) with the validation in place to confirm no regressions in normal operation.

### E2E Tests

Manual browser test: attempt to create a primitive with a very long name via the dashboard form. Verify the frontend validation catches it before sending. Verify the server also rejects it if sent via raw WebSocket.

## Monitoring Requirements

- `syncprim_validation_errors_total` counter with `reason` label exposed on `/metrics`.
- Alert: if `syncprim_validation_errors_total` rate > 10/minute, investigate for DoS attempt.

## Logging Requirements

On validation rejection:
```
level=WARN msg="message validation failed" reason="id_too_long" id_len=57344 conn_id="<connID>"
```
Do not log the actual id/name value.

## Metrics to Track

- `syncprim_validation_errors_total{reason="id_too_long"}` — counter
- `syncprim_validation_errors_total{reason="name_too_long"}` — counter
- `syncprim_validation_errors_total{reason="op_too_long"}` — counter

## Rollback Plan

Revert the validation function and constant definitions. The only side effect is that the new Prometheus counters disappear, which may cause a "no data" state in dashboards that reference them. No data corruption risk.

## Acceptance Criteria

- [ ] Messages with `id` > 256 bytes receive `{"type":"error"}` response
- [ ] Messages with `name` > 256 bytes receive `{"type":"error"}` response
- [ ] Messages with `id` = 256 bytes succeed (boundary condition)
- [ ] `syncprim_validation_errors_total` counters appear in `/metrics` output
- [ ] Frontend input fields validate length before sending
- [ ] All existing tests continue to pass

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage maintained ≥70%
- [ ] Monitoring in place (Prometheus counter)
- [ ] Documentation updated (CHANGELOG, README configuration table)
