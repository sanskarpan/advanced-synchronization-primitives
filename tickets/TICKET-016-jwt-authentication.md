# TICKET-016: JWT Authentication to Replace Static API Key

**Type:** feature
**Priority:** P2
**Estimate:** M (4 days)
**Epic:** Enterprise Features
**Labels:** p2, sprint-11, security, authentication, jwt
**Status:** TODO

## Problem Statement

The current static API key authentication has several fundamental limitations:
1. Single shared secret — all clients use the same key. Revoking access requires changing the key and restarting the server.
2. No per-user identity — the server cannot attribute operations to a specific user.
3. No expiry — API keys are valid indefinitely.
4. No scoping — all authenticated clients have full access to all operations.
5. No role differentiation — no admin/operator/viewer distinction.

These limitations make the current authentication system unsuitable for multi-user or multi-team deployments.

## Context

Current authentication in `web/server.go`:
```go
if s.cfg.APIKey != "" {
    // ... check Authorization: Bearer header
    if key != s.cfg.APIKey {
        http.Error(w, "Unauthorized", http.StatusUnauthorized)
        return
    }
}
```

The `Config.APIKey` field holds the shared secret. After TICKET-003, only `Authorization: Bearer <key>` is accepted.

JWTs (RFC 7519) are the industry standard for bearer token authentication. They encode claims (user identity, role, expiry) in a signed, verifiable payload without requiring server-side session storage.

## Goals

1. Accept `Authorization: Bearer <jwt>` where the JWT is signed with HMAC-SHA256.
2. JWT claims: `sub` (user ID, string), `role` (`admin`/`operator`/`viewer`), `exp` (expiry time), `iat` (issued-at time).
3. Server validates: signature, expiry, and required claims.
4. `Config.APIKey` becomes `Config.JWTSecret` (the HMAC secret used to sign tokens).
5. Backward compatibility: when `Config.JWTSecret` is empty, no authentication is required (same as current behavior).
6. Provide a `syncctl token generate` subcommand (TICKET-014) for generating tokens for testing.

## Non-Goals

- OAuth2 integration or authorization code flow.
- Token refresh endpoints.
- Storing user records (tokens are self-contained).
- RSA or ECDSA signatures (HMAC-SHA256 is sufficient for symmetric deployments).
- Role enforcement (that is TICKET-023 — this ticket only validates the JWT, not enforces roles).

## Technical Design

### JWT Structure

```
Header: {"alg": "HS256", "typ": "JWT"}
Payload: {
    "sub": "user@example.com",
    "role": "operator",
    "iat": 1746796800,
    "exp": 1746883200
}
```

### Implementation Without External Libraries

To avoid adding dependencies, implement minimal JWT validation:

```go
package auth

import (
    "crypto/hmac"
    "crypto/sha256"
    "encoding/base64"
    "encoding/json"
    "strings"
    "time"
    "fmt"
)

type Claims struct {
    Sub  string `json:"sub"`
    Role string `json:"role"`
    Iat  int64  `json:"iat"`
    Exp  int64  `json:"exp"`
}

func ValidateJWT(tokenStr, secret string) (*Claims, error) {
    parts := strings.Split(tokenStr, ".")
    if len(parts) != 3 {
        return nil, fmt.Errorf("invalid JWT format")
    }

    // Verify signature
    signingInput := parts[0] + "." + parts[1]
    mac := hmac.New(sha256.New, []byte(secret))
    mac.Write([]byte(signingInput))
    expectedSig := base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
    if !hmac.Equal([]byte(expectedSig), []byte(parts[2])) {
        return nil, fmt.Errorf("invalid signature")
    }

    // Decode payload
    payloadJSON, err := base64.RawURLEncoding.DecodeString(parts[1])
    if err != nil {
        return nil, fmt.Errorf("invalid payload encoding: %w", err)
    }

    var claims Claims
    if err := json.Unmarshal(payloadJSON, &claims); err != nil {
        return nil, fmt.Errorf("invalid payload JSON: %w", err)
    }

    // Validate expiry
    if claims.Exp == 0 {
        return nil, fmt.Errorf("missing exp claim")
    }
    if time.Now().Unix() > claims.Exp {
        return nil, fmt.Errorf("token expired")
    }

    // Validate sub
    if claims.Sub == "" {
        return nil, fmt.Errorf("missing sub claim")
    }

    return &claims, nil
}
```

### Config Changes

```go
type Config struct {
    // ... existing fields ...

    // JWTSecret, when non-empty, requires WebSocket clients to authenticate
    // with a valid HMAC-SHA256 signed JWT in Authorization: Bearer <jwt>.
    // The field replaces APIKey; if both are set, JWTSecret takes precedence.
    JWTSecret string

    // APIKey is deprecated. Use JWTSecret for new deployments.
    APIKey string
}
```

### HandleWebSocket Authentication Update

```go
func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
    if s.cfg.JWTSecret != "" {
        authHeader := r.Header.Get("Authorization")
        if !strings.HasPrefix(authHeader, "Bearer ") {
            http.Error(w, "Unauthorized: JWT required", http.StatusUnauthorized)
            return
        }
        tokenStr := strings.TrimPrefix(authHeader, "Bearer ")
        claims, err := auth.ValidateJWT(tokenStr, s.cfg.JWTSecret)
        if err != nil {
            slog.Warn("WebSocket auth failed", "err", err, "remote_addr", r.RemoteAddr)
            http.Error(w, "Unauthorized: "+err.Error(), http.StatusUnauthorized)
            return
        }
        // Attach claims to connection context for use by operation handlers
        r = r.WithContext(context.WithValue(r.Context(), contextKeyUser, claims))
        slog.Info("WebSocket authenticated", "sub", claims.Sub, "role", claims.Role)
    } else if s.cfg.APIKey != "" {
        // Legacy static API key fallback (deprecated, emit warning once)
        // ...
    }
    // ... rest of handler
}
```

## Backend Implementation

1. Create `internal/auth/jwt.go` with `ValidateJWT`, `Claims`, and a `GenerateJWT` helper for testing.
2. Create `internal/auth/jwt_test.go` with comprehensive tests.
3. Update `web/server.go` to use `auth.ValidateJWT` when `Config.JWTSecret` is set.
4. Update `Config` struct: add `JWTSecret string`. Deprecate `APIKey`.
5. Add `--jwt-secret` CLI flag to `cmd/server/main.go`.
6. Pass `Claims` via request context; future operation handlers can read the role for access control.
7. Add `contextKeyUser` type to avoid key collisions.

## Frontend Implementation

The browser dashboard does not use API key authentication directly. For deployment scenarios where the dashboard must authenticate:
1. Add a login form to the dashboard that accepts a JWT.
2. Store the JWT in `sessionStorage`.
3. Include it as `Authorization: Bearer <jwt>` using gorilla/websocket's custom header support (for the Go client) or a pre-handshake HTTP endpoint for browsers.

## Database / State Changes

None. JWT validation is stateless.

## API Changes

- New config field: `JWTSecret`.
- Deprecated config field: `APIKey` (still functional for one major release cycle).
- New CLI flag: `--jwt-secret`.
- Breaking: after a future release, `APIKey` support may be removed.

## Infrastructure Requirements

None.

## Edge Cases

- `exp` claim in the past: reject with `"token expired"`.
- `exp` claim 0 or missing: reject with `"missing exp claim"`.
- `iat` claim in the future: accept (clock skew tolerance not required for this implementation).
- `role` claim is empty or unknown: accept the token (role enforcement is TICKET-023). Log the unknown role.
- Extremely long JWT (malformed): the 64 KiB message size cap on WebSocket frames provides implicit protection for WS upgrade headers. HTTP header size limits (8 KiB by default in Go's `net/http`) protect the upgrade request header.

## Failure Handling

- Signature verification failure: 401, `"Unauthorized: invalid signature"`.
- Expired token: 401, `"Unauthorized: token expired"`.
- Malformed JWT: 401, `"Unauthorized: invalid JWT format"`.
- All auth failures: log at `slog.Warn` with `remote_addr` but never log the token value.

## Security Considerations

- HMAC key must be at least 256 bits (32 bytes) for adequate security. Warn if shorter.
- Do not log token values at any level.
- Use `hmac.Equal` (constant-time comparison) for signature comparison.
- The `GenerateJWT` helper in `internal/auth/` is for testing only; expose it only in test builds.
- Token expiry should be reasonable: 24 hours is a common default for developer tooling, 1 hour for production.

## Testing Plan

### Unit Tests

```go
func TestValidateJWT_Valid(t *testing.T) {
    token := generateTestToken("user@example.com", "operator", time.Now().Add(time.Hour))
    claims, err := auth.ValidateJWT(token, testSecret)
    require.NoError(t, err)
    assert.Equal(t, "user@example.com", claims.Sub)
    assert.Equal(t, "operator", claims.Role)
}

func TestValidateJWT_Expired(t *testing.T) {
    token := generateTestToken("user@example.com", "operator", time.Now().Add(-time.Hour))
    _, err := auth.ValidateJWT(token, testSecret)
    require.Error(t, err)
    assert.Contains(t, err.Error(), "expired")
}

func TestValidateJWT_InvalidSignature(t *testing.T) {
    // Tamper with payload, verify signature check fails
}

func TestWebSocketJWTAuthAccepted(t *testing.T) {
    // Configure server with JWTSecret
    // Connect with valid JWT
    // Assert WebSocket connection established
}

func TestWebSocketJWTAuthRejectedExpired(t *testing.T) {
    // Connect with expired JWT
    // Assert HTTP 401
}
```

### Integration Tests

Full lifecycle test with JWT: generate token → connect → create primitive → operate → delete.

### E2E Tests

Manual: generate a JWT using `syncctl token generate --secret <s> --sub user --role operator --exp 1h`. Connect using the dashboard with the token in the Authorization header. Verify connected successfully.

## Monitoring Requirements

- Log successful authentications at INFO: `sub` and `role` (never the token).
- Log failures at WARN: `remote_addr` and error reason.

## Metrics to Track

- `syncprim_auth_failures_total{reason="expired"|"invalid_signature"|"malformed"}` — new counter.

## Rollback Plan

Remove `JWTSecret` from Config and revert the `HandleWebSocket` authentication block. Re-enable `APIKey` as the primary authentication mechanism. No client data is affected.

## Acceptance Criteria

- [ ] Valid unexpired JWT accepted for WebSocket upgrade
- [ ] Expired JWT rejected with HTTP 401
- [ ] Invalid signature rejected with HTTP 401
- [ ] Claims (`sub`, `role`) available in connection context
- [ ] `Config.JWTSecret` field and `--jwt-secret` CLI flag work
- [ ] `Config.APIKey` still works (backward compatibility, with deprecation log)
- [ ] `hmac.Equal` used for constant-time comparison
- [ ] No token values logged

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage ≥70%
- [ ] Security review by a second engineer
- [ ] README authentication section updated
- [ ] CHANGELOG entry with migration guide from APIKey to JWTSecret
