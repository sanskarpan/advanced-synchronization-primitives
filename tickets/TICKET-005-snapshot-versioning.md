# TICKET-005: Add Version Field to Snapshot Persistence Format

**Type:** improvement
**Priority:** P0
**Estimate:** S (1–2 days)
**Epic:** Security and Stability Hardening
**Labels:** p0, sprint-1, persistence, web-server
**Status:** SHIPPED

**Tracking Note:** Implemented on `origin/main`. This ticket is retained as historical planning context; the design notes and checklists below may not match the final shipped implementation verbatim.


## Problem Statement

The JSON snapshot file written by the server when `Config.SnapshotPath` is set has no version field. The file is a flat JSON object containing primitive state. When the snapshot schema changes in a future release (new fields, renamed fields, or restructured types), the server has no way to detect that it is loading an incompatible snapshot. It will silently attempt to deserialize the old format into the new struct, potentially:

1. Ignoring new required fields (zero-value defaults may be semantically wrong).
2. Failing to deserialize with a JSON error (logged as a warning, but the error is cryptic).
3. Loading partially corrupt state that causes runtime panics later.

Snapshot versioning is a standard practice for any persisted format.

## Context

Current snapshot behavior in `web/server.go`:
- On startup, if `Config.SnapshotPath` is set, the server calls `loadSnapshot()`.
- `loadSnapshot()` reads the file with `os.ReadFile` and unmarshals into `map[string]snapshotPrimitive`.
- On shutdown, `saveSnapshot()` marshals the current state to JSON and writes the file.

The `snapshotPrimitive` struct:
```go
type snapshotPrimitive struct {
    Type     string          `json:"type"`
    Name     string          `json:"name"`
    Capacity int32           `json:"capacity,omitempty"`
    Parties  int32           `json:"parties,omitempty"`
}
```

There is no envelope or version field. The file looks like:
```json
{
  "mutex-1": {"type": "Mutex", "name": "db-lock"},
  "sem-1": {"type": "Semaphore", "name": "rate-limiter", "capacity": 10}
}
```

## Goals

1. Add a versioned envelope to the snapshot format: `{"version": 1, "primitives": {...}}`.
2. On load, check the version field. If missing (legacy format), attempt to load in backward-compatible mode with a `slog.Warn`.
3. If the version is present but not `1` (future version), log a warning and skip loading (fail safe to empty state).
4. Write all new snapshots in the versioned format.
5. Define the current version as a named constant.

## Non-Goals

- Migrating between snapshot schema versions (only version 1 exists currently).
- Encrypting the snapshot file.
- Handling corrupted snapshot files (JSON parse errors already result in a warning and empty state).

## Technical Design

Define a new envelope type:
```go
const snapshotVersion = 1

type snapshotEnvelope struct {
    Version    int                           `json:"version"`
    Primitives map[string]snapshotPrimitive  `json:"primitives"`
}
```

Update `saveSnapshot`:
```go
func (s *Server) saveSnapshot() {
    envelope := snapshotEnvelope{
        Version:    snapshotVersion,
        Primitives: s.buildSnapshotMap(),
    }
    data, err := json.MarshalIndent(envelope, "", "  ")
    // ... write to file
}
```

Update `loadSnapshot` with backward compatibility:
```go
func (s *Server) loadSnapshot() {
    data, err := os.ReadFile(s.cfg.SnapshotPath)
    if err != nil {
        // Missing file is normal on first run
        if !os.IsNotExist(err) {
            slog.Warn("failed to read snapshot file", "err", err)
        }
        return
    }

    // Try versioned format first
    var envelope snapshotEnvelope
    if err := json.Unmarshal(data, &envelope); err == nil && envelope.Version > 0 {
        if envelope.Version != snapshotVersion {
            slog.Warn("snapshot version mismatch; skipping load (safe fallback to empty state)",
                "file_version", envelope.Version,
                "server_version", snapshotVersion)
            return
        }
        s.restoreFromSnapshot(envelope.Primitives)
        slog.Info("loaded snapshot", "version", envelope.Version,
            "primitives", len(envelope.Primitives))
        return
    }

    // Fall back to legacy unversioned format
    var legacy map[string]snapshotPrimitive
    if err := json.Unmarshal(data, &legacy); err != nil {
        slog.Warn("failed to parse snapshot file", "err", err)
        return
    }
    slog.Warn("loaded snapshot in legacy unversioned format; will be upgraded on next shutdown",
        "primitives", len(legacy))
    s.restoreFromSnapshot(legacy)
}
```

## Backend Implementation

1. Define `snapshotVersion = 1` constant.
2. Define `snapshotEnvelope` struct.
3. Update `saveSnapshot` to write the envelope.
4. Update `loadSnapshot` with the two-pass (versioned first, legacy fallback) logic described above.
5. Add test `TestSnapshotVersioning` that:
   a. Creates primitives, saves snapshot.
   b. Reads snapshot file, asserts `"version": 1` is present.
   c. Starts a new server pointing at the same snapshot, asserts primitives are restored.
6. Add test `TestSnapshotLegacyFormatBackwardCompatibility` that:
   a. Writes a legacy unversioned snapshot file directly.
   b. Starts server, asserts primitives are loaded.
   c. After shutdown, asserts the snapshot file now contains the versioned format.
7. Add test `TestSnapshotFutureVersionSkipped` that:
   a. Writes `{"version": 99, "primitives": {...}}` to the snapshot file.
   b. Starts server, asserts no primitives are loaded (empty state).
   c. Asserts a warning was logged.

## Frontend Implementation

None. Snapshot management is server-side.

## Database / State Changes

The snapshot file format changes. The new format is:
```json
{
  "version": 1,
  "primitives": {
    "mutex-1": {"type": "Mutex", "name": "db-lock"},
    "sem-1": {"type": "Semaphore", "name": "rate-limiter", "capacity": 10}
  }
}
```

Existing snapshot files without `"version"` are still loadable (backward compatibility).

## API Changes

None. Snapshot persistence is transparent to WebSocket clients.

## Infrastructure Requirements

None.

## Edge Cases

- Snapshot file does not exist: `os.IsNotExist` → silent, expected on first run.
- Snapshot file is empty: JSON parse error → warn and skip.
- Snapshot file is valid JSON but not a snapshot (e.g., `{}`): `envelope.Version == 0` → falls through to legacy format attempt → empty map → no primitives loaded, no error.
- Snapshot file has `"version": 1` but `"primitives"` is null: `restoreFromSnapshot(nil)` → no primitives, no panic.
- Server is killed (SIGKILL) mid-write: `os.WriteFile` is atomic on most platforms (writes to a temp file and renames). However, an interrupted rename can leave a partial file. The next startup will fail to parse the partial file and start with empty state (safe fallback).

## Failure Handling

All failure modes fall back to empty state with a `slog.Warn`. No operation should cause a panic or exit. The server is fully functional even with an unloadable snapshot.

## Security Considerations

- The snapshot file contains primitive IDs and names which are user-controlled strings. If the snapshot file is stored in a world-readable location, this could leak information about the server's usage. Document that `SnapshotPath` should use restrictive file permissions (`0600`).
- No secrets are stored in the snapshot (no API keys, no user data beyond primitive IDs/names).

## Testing Plan

### Unit Tests

As described in Backend Implementation (3 tests).

### Integration Tests

Add snapshot round-trip test to `web/server_test.go`: create server with `SnapshotPath`, create primitives via WebSocket, call `Shutdown`, restart server with same `SnapshotPath`, verify primitives are present.

### E2E Tests

Manual: start server with `-snapshot /tmp/syncprim.json`, create several primitives via dashboard, stop server with SIGTERM, verify `/tmp/syncprim.json` contains `"version": 1`, restart server, verify dashboard shows the primitives still exist.

## Monitoring Requirements

Log the snapshot load result at `slog.Info` level so operators can confirm persistence is working:
```
level=INFO msg="loaded snapshot" version=1 primitives=5
```

## Logging Requirements

```
level=INFO  msg="loaded snapshot" version=1 primitives=5
level=WARN  msg="loaded snapshot in legacy unversioned format; will be upgraded on next shutdown" primitives=3
level=WARN  msg="snapshot version mismatch; skipping load" file_version=99 server_version=1
level=WARN  msg="failed to parse snapshot file" err="<json error>"
level=INFO  msg="saved snapshot" version=1 primitives=5 path="/data/syncprim.json"
```

## Metrics to Track

None new for this ticket.

## Rollback Plan

Revert `saveSnapshot` to write the unversioned format. The `loadSnapshot` backward-compatibility path will still be present to read versioned files written before the rollback, but since the rollback removes the versioned writer, subsequent snapshots will be unversioned again. No data loss.

## Acceptance Criteria

- [ ] New snapshot files contain `{"version": 1, "primitives": {...}}`
- [ ] Legacy unversioned snapshot files are loaded with a warning
- [ ] Snapshot files with `"version": 99` are skipped with a warning and server starts with empty state
- [ ] `snapshotVersion` is a named constant (not a magic number `1` inline)
- [ ] Tests for all three cases (new format, legacy format, future version) pass

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage maintained ≥70%
- [ ] Documentation updated (README configuration table, snapshot path description)
- [ ] CHANGELOG entry written
