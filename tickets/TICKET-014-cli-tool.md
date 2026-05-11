# TICKET-014: CLI Tool for Manual Primitive Management

**Type:** feature
**Priority:** P2
**Estimate:** M (4 days)
**Epic:** Developer Experience
**Labels:** p2, sprint-7, cli, developer-experience, tooling
**Status:** SHIPPED

**Tracking Note:** Implemented on `origin/main`. This ticket is retained as historical planning context; the design notes and checklists below may not match the final shipped implementation verbatim.


## Problem Statement

Operators and developers have no command-line interface for managing synchronization primitives. The only interaction modes are:
1. The browser dashboard (requires a graphical environment).
2. Raw WebSocket with manually crafted JSON (requires a WebSocket client tool and knowledge of the JSON protocol).

A CLI tool enables:
- Scripting primitive management in CI/CD pipelines.
- Debugging stuck locks in production without a browser.
- Integration with shell-based monitoring scripts.
- Quick ad-hoc testing during development.

## Context

The Go SDK client (`pkg/client/`) built in TICKET-013 provides all the necessary functionality. The CLI is a thin wrapper over the SDK.

## Goals

1. Create `cmd/syncctl/main.go` with subcommand-based CLI.
2. Commands: `list`, `create`, `op`, `delete`, `stats`.
3. Authentication via `--api-key` flag or `SYNCPRIM_API_KEY` env var.
4. Server URL via `--server` flag or `SYNCPRIM_SERVER` env var (default: `ws://localhost:8085/ws`).
5. JSON output mode (`--json`) for machine-readable output.
6. Human-readable tabular output by default.

## Non-Goals

- Interactive TUI (a future ticket).
- Remote configuration of the server (configuration is only via startup flags).
- Bulk operations (importing/exporting many primitives).

## Technical Design

### Command Structure

```
syncctl [--server <url>] [--api-key <key>] [--json] <command>

Commands:
  list                    List all primitives
  create <type> <id>      Create a new primitive
  op <id> <operation>     Execute an operation on a primitive
  delete <id>             Delete a primitive
  stats <id>              Show detailed statistics for a primitive
  version                 Print the syncctl version
  help                    Show help
```

### Global Flags

```
  --server <url>    WebSocket server URL (default: ws://localhost:8085/ws)
                    Env: SYNCPRIM_SERVER
  --api-key <key>   Bearer API key (default: empty = no auth)
                    Env: SYNCPRIM_API_KEY
  --json            Output in JSON format (default: tabular)
  --timeout <dur>   Operation timeout (default: 30s)
  --insecure        Accept invalid TLS certificates (use with caution)
```

### `list` Command

```
$ syncctl list
ID               TYPE        NAME              STATE
mutex-1          Mutex       db-lock           locked
sem-1            Semaphore   rate-limiter      3/10 available
barrier-1        Barrier     phase-1           2/4 arrived
```

With `--json`:
```json
[
  {"id": "mutex-1", "type": "Mutex", "name": "db-lock", "state": "locked"},
  ...
]
```

### `create` Command

```
$ syncctl create mutex my-lock
Created: my-lock (Mutex)

$ syncctl create semaphore rate-limiter --capacity 10
Created: rate-limiter (Semaphore, capacity=10)

$ syncctl create barrier phase-sync --parties 4
Created: phase-sync (Barrier, parties=4)
```

Required flags by type:
- `semaphore`: `--capacity <int32>`
- `barrier`: `--parties <int32>`
- Others: no additional flags

### `op` Command

```
$ syncctl op my-lock lock --hold 1000
Locked: my-lock (will auto-release in 1000ms)

$ syncctl op my-lock unlock
Unlocked: my-lock

$ syncctl op sem-1 acquire --hold 5000
Acquired: sem-1 (will auto-release in 5000ms)
```

### `delete` Command

```
$ syncctl delete my-lock
Deleted: my-lock
```

### `stats` Command

```
$ syncctl stats mutex-1
Primitive: mutex-1
Type:      Mutex
Name:      db-lock
State:     locked
Created:   2026-05-09T10:23:00Z (2m30s ago)

Metrics:
  Locks:           4,201
  Unlocks:         4,200
  Waits:           312
  Avg Wait Time:   1.2ms
  Waiters Queued:  1
```

## Backend Implementation

1. Create `cmd/syncctl/main.go`.
2. Parse global flags using `flag` package.
3. Parse subcommand using the second positional argument after flags.
4. Use `pkg/client.Client` for all server communication (TICKET-013 must be done first).
5. Implement tabular output using `text/tabwriter`.
6. Implement JSON output using `encoding/json`.
7. Exit codes: 0 = success, 1 = server error, 2 = usage error.
8. Add `syncctl` as a build target in `Makefile`:
   ```makefile
   syncctl:
       go build -o syncctl ./cmd/syncctl
   ```

## Frontend Implementation

None.

## Database / State Changes

None.

## API Changes

None. The CLI uses the existing WebSocket protocol via the SDK.

## Infrastructure Requirements

The CLI binary must be cross-compilable for Linux, macOS, and Windows:
```makefile
dist:
    GOOS=linux GOARCH=amd64 go build -o dist/syncctl-linux-amd64 ./cmd/syncctl
    GOOS=darwin GOARCH=arm64 go build -o dist/syncctl-darwin-arm64 ./cmd/syncctl
    GOOS=windows GOARCH=amd64 go build -o dist/syncctl-windows-amd64.exe ./cmd/syncctl
```

## Edge Cases

- Server not running: connection error → print `"syncctl: failed to connect to <url>: <err>"` and exit 1.
- `delete` on non-existent primitive: server returns error → print error message and exit 1.
- `op` on wrong primitive type (e.g., `lock` on a barrier): server returns error → print error and exit 1.
- Timeout: `--timeout 5s` flag controls how long to wait for a server response. After timeout, exit 1 with "operation timed out".
- `--json` mode: all output goes to stdout as JSON. Error messages go to stderr. Exit codes still apply.

## Failure Handling

- Connection failure: non-zero exit, error to stderr.
- Server error response: non-zero exit, error message to stderr.
- Usage error (wrong flags): help text to stderr, exit 2.
- Signal interrupt (SIGINT): send `Close` frame to server, exit 0.

## Security Considerations

- The `--api-key` flag value may appear in process listings (e.g., `ps aux`). Document that `SYNCPRIM_API_KEY` env var should be used in production scripts.
- The CLI does not cache or store credentials.
- Do not print the API key in any output or error messages.

## Testing Plan

### Unit Tests

```go
func TestListCommand(t *testing.T) {
    // Start test server, create 2 primitives
    // Run syncctl list --server <url> --json
    // Parse JSON output, assert 2 primitives returned
}

func TestCreateAndDeleteMutex(t *testing.T) {
    // Run syncctl create mutex test-lock
    // Assert exit code 0
    // Run syncctl delete test-lock
    // Assert exit code 0
}

func TestConnectFailure(t *testing.T) {
    // Run syncctl list --server ws://localhost:9999/ws
    // Assert exit code 1
    // Assert stderr contains "failed to connect"
}
```

### Integration Tests

End-to-end test that runs the compiled `syncctl` binary against a live test server. Test all subcommands.

### E2E Tests

Manual: start the server, run a series of `syncctl` commands, verify output matches expectations. Test with `--json` flag and pipe to `jq`.

## Monitoring Requirements

None. The CLI is a client tool.

## Logging Requirements

- Verbose mode (`--verbose` or `-v`): log all WebSocket messages to stderr.
- No logging by default.

## Metrics to Track

None.

## Rollback Plan

Remove `cmd/syncctl/`. The main server binary is unaffected.

## Acceptance Criteria

- [ ] `syncctl list` outputs all primitives from the server
- [ ] `syncctl create mutex my-lock` creates a mutex and outputs confirmation
- [ ] `syncctl op my-lock lock` locks the mutex and outputs confirmation
- [ ] `syncctl delete my-lock` deletes the mutex and outputs confirmation
- [ ] `syncctl stats my-lock` shows detailed statistics
- [ ] `--json` flag produces valid JSON output
- [ ] Connection failure exits with code 1 and informative message
- [ ] API key auth works via `--api-key` and `SYNCPRIM_API_KEY` env var
- [ ] Cross-platform builds succeed (Linux, macOS, Windows)

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Binary cross-compiles for linux/amd64, darwin/arm64, windows/amd64
- [ ] README updated with CLI installation and usage examples
- [ ] CHANGELOG entry written
