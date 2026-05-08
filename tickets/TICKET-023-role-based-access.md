# TICKET-023: Role-Based Access Control

**Type:** feature
**Priority:** P2
**Estimate:** M (4 days)
**Epic:** Enterprise Features
**Labels:** p2, sprint-11, security, rbac, authorization
**Status:** TODO

## Problem Statement

Authentication (TICKET-016) verifies who the user is. Authorization determines what they can do. Without role enforcement, every authenticated user can perform every operation including deleting primitives that other users are using.

Role-based access control (RBAC) allows organizations to grant least-privilege access: monitoring users can only read, application operators can acquire/release but cannot create or delete, and admins can manage the full lifecycle.

## Context

TICKET-016 adds a `role` claim to the JWT payload. This ticket enforces that role in the message dispatch layer.

Three roles:
- `admin`: full access — create, delete, operate
- `operator`: operate existing primitives — acquire, release, lock, unlock, etc. Cannot create or delete.
- `viewer`: read-only — receive stats broadcasts. Cannot send create, delete, or op messages.

## Goals

1. Define three roles: `admin`, `operator`, `viewer`.
2. Enforce role permissions in `HandleWebSocket` message dispatch.
3. Return HTTP 403 Forbidden (or WebSocket error) for operations not permitted by the role.
4. When JWT auth is disabled, apply the `admin` role by default.
5. Add tests for each role boundary.

## Non-Goals

- Per-primitive role assignments (only global role per connection).
- Role management API (roles are encoded in JWTs).
- Fine-grained per-operation role configuration.

## Technical Design

### Role Constants

```go
const (
    RoleAdmin    = "admin"
    RoleOperator = "operator"
    RoleViewer   = "viewer"
)
```

### Permission Matrix

| Operation | Admin | Operator | Viewer |
|-----------|-------|----------|--------|
| Receive state/update broadcasts | YES | YES | YES |
| `createRWLock`, `createMutex`, etc. | YES | NO | NO |
| `primitiveOp` (any operation) | YES | YES | NO |
| `deletePrimitive` | YES | NO | NO |
| `requestFullRefresh` | YES | YES | YES |

### Enforcement

In the message dispatch switch:
```go
role := getRoleFromContext(r.Context()) // set during JWT validation

switch msg.Type {
case "createRWLock", "createMutex", "createSemaphore",
     "createCondVar", "createBarrier", "createWaitGroup",
     "createOnce", "createSingleflight":
    if role != RoleAdmin {
        sendError(conn, "forbidden: create operations require admin role")
        continue
    }

case "deletePrimitive":
    if role != RoleAdmin {
        sendError(conn, "forbidden: delete operations require admin role")
        continue
    }

case "primitiveOp":
    if role == RoleViewer {
        sendError(conn, "forbidden: operations not permitted for viewer role")
        continue
    }
}
```

### Context Key for Role

```go
type contextKey string
const contextKeyRole contextKey = "role"

func getRoleFromContext(ctx context.Context) string {
    role, _ := ctx.Value(contextKeyRole).(string)
    if role == "" {
        return RoleAdmin // default when no auth
    }
    return role
}
```

In the JWT auth handler, store the role in context:
```go
r = r.WithContext(context.WithValue(r.Context(), contextKeyRole, claims.Role))
```

## Backend Implementation

1. Define role constants and permission checks.
2. Update `HandleWebSocket` to read the role from context.
3. Add role enforcement in the message dispatch switch.
4. Add tests:
   - `TestViewerCannotCreatePrimitive`
   - `TestViewerCannotCallPrimitiveOp`
   - `TestOperatorCannotCreatePrimitive`
   - `TestOperatorCanOperateExistingPrimitive`
   - `TestAdminCanDoEverything`
   - `TestDefaultRoleIsAdminWhenNoAuth`

## Frontend Implementation

When the user is a viewer:
- Disable all create/delete buttons.
- Disable all operation buttons.
- Show a badge "Viewer mode" in the header.

When the user is an operator:
- Disable create/delete buttons.
- Enable operation buttons.
- Show a badge "Operator mode" in the header.

The dashboard detects its role from the JWT claims. The JWT can be decoded (not verified) in the browser to extract the role claim.

## Database / State Changes

None.

## API Changes

New error responses for unauthorized operations:
```json
{"type": "error", "payload": {"message": "forbidden: create operations require admin role"}}
```

This does not close the connection — the client can still observe state and receive broadcasts.

## Infrastructure Requirements

None.

## Edge Cases

- JWT has an unknown role (e.g., `"superuser"`): treat as `viewer` (most restrictive). Log a warning.
- JWT has no role claim: treat as `viewer`.
- No JWT (auth disabled): treat as `admin`.
- Role downgrade during a connection: not possible (role is set at connection time from the JWT and does not change).

## Failure Handling

- Forbidden operation: send error message, continue connection.
- Unknown role: default to `viewer`, log warning.

## Security Considerations

- The role enforcement is the final authorization gate. Ensure it is applied BEFORE any state mutation.
- Context key uses a typed `contextKey` string type to prevent key collisions with other context values.
- Do not leak role information in error messages to viewers (e.g., don't say "admin role required" if you don't want viewers to know the role structure). Alternatively, it's fine to be transparent — obscurity is not security.

## Testing Plan

### Unit Tests

```go
func TestViewerCannotCreatePrimitive(t *testing.T) {
    // Configure server with JWT secret
    // Connect with viewer JWT
    // Send createMutex
    // Assert error response with "forbidden"
}

func TestOperatorCanOperateExistingPrimitive(t *testing.T) {
    // Connect with admin JWT, create mutex
    // Connect a second client with operator JWT
    // Operator client locks the mutex
    // Assert success
}

func TestViewerReceivesBroadcasts(t *testing.T) {
    // Connect viewer client
    // Verify it receives update messages
}
```

### Integration Tests

Full lifecycle test: admin creates primitive, operator operates it, viewer receives updates.

### E2E Tests

Manual: generate viewer JWT, connect to dashboard, verify all create/delete/op buttons are disabled.

## Monitoring Requirements

- Log role enforcement decisions at DEBUG level.
- `syncprim_auth_forbidden_total{role="viewer",operation="create"}` — future metric.

## Logging Requirements

```
level=DEBUG msg="RBAC: operation permitted" role="operator" op="lock" id="mutex-1"
level=INFO  msg="RBAC: operation denied" role="viewer" op="lock" conn_id="c1"
level=WARN  msg="unknown role in JWT; defaulting to viewer" role="superuser"
```

## Metrics to Track

Future: `syncprim_rbac_denied_total{role, operation}` counter.

## Rollback Plan

Remove role enforcement from the dispatch switch. All authenticated users have full access again (effectively admin). No data loss.

## Acceptance Criteria

- [ ] Viewer JWT cannot create or delete primitives (HTTP error response)
- [ ] Viewer JWT cannot call primitiveOp (HTTP error response)
- [ ] Viewer JWT receives state broadcasts
- [ ] Operator JWT can call primitiveOp
- [ ] Operator JWT cannot create or delete
- [ ] Admin JWT has full access
- [ ] No-auth mode defaults to admin
- [ ] Unknown role defaults to viewer with a warning log

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage ≥70%
- [ ] README authentication section updated with role descriptions
- [ ] CHANGELOG entry written
