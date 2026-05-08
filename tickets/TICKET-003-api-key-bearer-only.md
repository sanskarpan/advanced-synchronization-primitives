# TICKET-003: Remove API Key from URL Query Parameter — Bearer Header Only

**Type:** security
**Priority:** P0
**Estimate:** S (1 day)
**Epic:** Security and Stability Hardening
**Labels:** security, p0, sprint-1, web-server, breaking-change
**Status:** TODO

## Problem Statement

The server currently accepts WebSocket API key authentication via two mechanisms:
1. `Authorization: Bearer <key>` HTTP header (secure)
2. `?key=<apikey>` URL query parameter (insecure)

URL query parameters are logged by every tier in a typical web infrastructure stack: web server access logs, reverse proxy logs (nginx, Caddy, AWS ALB), API gateways, CDN edge nodes, and browser history. This means the API key is stored in plain text in multiple locations outside the operator's control.

**Concrete attack scenario:** An operator deploys the server behind nginx. nginx's `access_log` directive includes the full request URI by default. Every WebSocket connection using `?key=<secret>` writes the secret to `access.log`. If that log file is exfiltrated (via log aggregation, backup, or a path traversal vulnerability), the API key is compromised.

The OWASP API Security guidelines (API8:2023 — Security Misconfiguration) explicitly warn against credentials in URL query parameters.

## Context

In `web/server.go`, `HandleWebSocket` contains:
```go
func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
    // API key authentication
    if s.cfg.APIKey != "" {
        authHeader := r.Header.Get("Authorization")
        key := ""
        if strings.HasPrefix(authHeader, "Bearer ") {
            key = strings.TrimPrefix(authHeader, "Bearer ")
        } else {
            // Fallback: accept key via query parameter
            key = r.URL.Query().Get("key")
        }
        if key != s.cfg.APIKey {
            http.Error(w, "Unauthorized", http.StatusUnauthorized)
            return
        }
    }
```

The `?key=<apikey>` path is the `else` branch. It must be removed entirely.

## Goals

1. Remove the `r.URL.Query().Get("key")` authentication path.
2. Ensure clients using `Authorization: Bearer <key>` are unaffected.
3. Return HTTP 401 with a clear error message when no valid `Authorization: Bearer` header is present.
4. Add a `slog.Warn` when a request arrives with `?key=` in the URL but no `Authorization` header (helps operators identify clients that need to migrate).
5. Document the migration in CHANGELOG and README.

## Non-Goals

- Implementing JWT authentication (TICKET-016).
- Changing the API key configuration mechanism (`-api-key` flag or `Config.APIKey` field).
- Rate-limiting authentication failures (future work).

## Technical Design

Remove the `else` branch entirely from the authentication block:

```go
if s.cfg.APIKey != "" {
    authHeader := r.Header.Get("Authorization")
    if !strings.HasPrefix(authHeader, "Bearer ") {
        // Log if the client attempted the deprecated ?key= path
        if r.URL.Query().Get("key") != "" {
            slog.Warn("WebSocket auth rejected: client used deprecated ?key= parameter; " +
                "migrate to Authorization: Bearer <key>",
                "remote_addr", r.RemoteAddr)
        }
        http.Error(w, "Unauthorized: use Authorization: Bearer <key>", http.StatusUnauthorized)
        return
    }
    key := strings.TrimPrefix(authHeader, "Bearer ")
    if key != s.cfg.APIKey {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }
}
```

Note: The warning log for the deprecated `?key=` path does NOT log the key value itself, only the fact that the deprecated path was attempted. This avoids logging the key in the new code path while still helping operators identify misconfigured clients.

## Backend Implementation

1. Remove the `else { key = r.URL.Query().Get("key") }` block.
2. Add the deprecation warning log (see Technical Design above).
3. Update the inline comment to document that Bearer header is the only accepted mechanism.
4. Add a test `TestWebSocketAuthQueryParamRejected` that verifies `?key=<valid-key>` returns 401.
5. Add a test `TestWebSocketAuthBearerAccepted` that verifies `Authorization: Bearer <valid-key>` succeeds.

## Frontend Implementation

The dashboard frontend does not use API key authentication directly (it connects to the same origin without an API key in development). However, update the README and any documentation examples that show WebSocket connection strings.

If the dashboard ever needs to support API key authentication (e.g., when connecting to a remote server), it must use a custom header, which gorilla/websocket supports via `websocket.Dialer.Header`.

## Database / State Changes

None.

## API Changes

**BREAKING CHANGE:** `?key=<apikey>` query parameter is no longer accepted for authentication. Clients must use `Authorization: Bearer <key>`.

Migration guide for affected clients:

**Go (using gorilla/websocket):**
```go
// Before (deprecated):
conn, _, err := websocket.DefaultDialer.Dial("ws://host/ws?key=secret", nil)

// After:
header := http.Header{"Authorization": {"Bearer secret"}}
conn, _, err := websocket.DefaultDialer.Dial("ws://host/ws", header)
```

**JavaScript (browser WebSocket API):**
The browser WebSocket API does not support custom headers. Use a ticket-based authentication flow: exchange the API key for a short-lived token via an HTTP endpoint, then pass the token as a query parameter.

Alternatively, configure the server without an API key for browser-based access and rely on origin-based access control (`Config.AllowedOrigins`).

## Infrastructure Requirements

None.

## Edge Cases

- Client sends both `?key=<key>` and `Authorization: Bearer <key>`: The Bearer header takes precedence and authentication succeeds. The deprecated warning is NOT logged because the `Authorization` header was present. This provides a smooth migration window.
- Client sends `Authorization: Bearer ` (empty key after "Bearer "): `key == ""`. If `s.cfg.APIKey == ""`, this is fine (no auth required). If `s.cfg.APIKey != ""`, this fails with 401.
- Server configured with empty `APIKey` (`Config.APIKey == ""`): authentication is disabled entirely. Both code paths (header and query param) are bypassed. No behavior change.

## Failure Handling

- Invalid/missing token: HTTP 401. No connection upgrade.
- Network error during HTTP 401 write: logged, connection dropped.

## Security Considerations

- **Timing attack**: The key comparison `key != s.cfg.APIKey` is not constant-time. Use `subtle.ConstantTimeCompare` to prevent timing-based key enumeration: `subtle.ConstantTimeCompare([]byte(key), []byte(s.cfg.APIKey)) != 1`. This is a defense-in-depth improvement.
- **Do not log the key value** at any log level, including DEBUG.
- The deprecation warning log (`?key= attempted`) intentionally does not log the key value.

## Testing Plan

### Unit Tests

In `web/server_test.go`:

```go
func TestWebSocketAuthQueryParamRejected(t *testing.T) {
    // Configure server with APIKey = "test-secret"
    // Connect to /ws?key=test-secret (no Authorization header)
    // Assert HTTP 401 response
    // Assert WebSocket connection was NOT established
}

func TestWebSocketAuthBearerAccepted(t *testing.T) {
    // Configure server with APIKey = "test-secret"
    // Connect with Authorization: Bearer test-secret
    // Assert WebSocket connection established successfully
}

func TestWebSocketAuthBearerWrongKey(t *testing.T) {
    // Configure server with APIKey = "test-secret"
    // Connect with Authorization: Bearer wrong-key
    // Assert HTTP 401 response
}

func TestWebSocketAuthNoKeyConfigured(t *testing.T) {
    // Configure server with APIKey = ""
    // Connect without any Authorization header
    // Assert WebSocket connection established (no auth required)
}

func TestWebSocketAuthDeprecatedParamWarningLogged(t *testing.T) {
    // Configure server with APIKey = "test-secret"
    // Connect with ?key=test-secret (no Authorization header)
    // Capture slog output, assert warning message is logged
    // Assert HTTP 401 response
}
```

### Integration Tests

Run the existing `TestWebSocketLoad` with no API key configured. Verify no regressions.

### E2E Tests

Deploy with `--api-key my-secret`. Attempt to connect from the browser dashboard. Verify the dashboard shows an "Unauthorized" error. Configure the browser-side connection to use the `Authorization` header (development scenario).

## Monitoring Requirements

No new metrics. The deprecation warning log provides operator visibility via log aggregation.

## Logging Requirements

On deprecated `?key=` attempt:
```
level=WARN msg="WebSocket auth rejected: client used deprecated ?key= parameter" remote_addr="1.2.3.4:12345"
```

On successful Bearer auth (optional, DEBUG level):
```
level=DEBUG msg="WebSocket authenticated" remote_addr="1.2.3.4:12345"
```

## Metrics to Track

Future: add `syncprim_auth_failures_total` counter. Not in scope for this ticket.

## Rollback Plan

Revert the authentication block change. Clients using `?key=` will resume working immediately. No data corruption. Server restart required.

## Acceptance Criteria

- [ ] Connecting with `?key=<valid-key>` returns HTTP 401
- [ ] Connecting with `Authorization: Bearer <valid-key>` succeeds
- [ ] Connecting with `Authorization: Bearer <wrong-key>` returns HTTP 401
- [ ] A `slog.Warn` is emitted when `?key=` is detected (without logging the key value)
- [ ] Key comparison uses `subtle.ConstantTimeCompare`
- [ ] CHANGELOG entry documents the breaking change with migration guide
- [ ] README authentication section updated

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage maintained ≥70%
- [ ] CHANGELOG entry written
- [ ] README updated
- [ ] No key values logged at any level
