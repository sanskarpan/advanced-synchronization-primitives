# TICKET-022: Multi-Tenant Namespacing

**Type:** feature
**Priority:** P2
**Estimate:** L (1–2 weeks)
**Epic:** Enterprise Features
**Labels:** p2, sprint-11, multi-tenancy, architecture, breaking-change
**Status:** SHIPPED

**Tracking Note:** Implemented on `origin/main`. This ticket is retained as historical planning context; the design notes and checklists below may not match the final shipped implementation verbatim.


## Problem Statement

Currently, each WebSocket connection manages its own isolated set of primitives. No two connections can share a primitive instance. This limits the use cases:
1. Multiple microservices cannot share a distributed lock.
2. CI jobs running in parallel cannot synchronize via a shared barrier.
3. There is no concept of a "team workspace" where multiple users access the same set of primitives.

Multi-tenant namespacing allows:
1. Multiple connections to join the same namespace and share primitive state.
2. Each namespace has isolated primitive storage.
3. A namespace can be configured with access controls (TICKET-023).

## Context

Current architecture: each connection creates primitives in its own local map (`map[string]primEntry`). Primitive IDs must be unique within a connection, but different connections can use the same ID without conflict.

After this change: primitives live in a namespace store shared by all connections in the same namespace. Primitive IDs must be unique within a namespace.

## Goals

1. Add `namespace` claim to the WebSocket authentication (either via JWT claim or a `namespace` query parameter for unauthenticated mode).
2. Create a `NamespaceRegistry` that manages one `Namespace` per namespace name.
3. Each `Namespace` holds a `map[string]primEntry` protected by a mutex.
4. Connections in the same namespace share primitive state.
5. Primitives created by a connection in namespace A are visible to all connections in namespace A.
6. Namespace A primitives are invisible to namespace B connections.

## Non-Goals

- Cross-namespace primitive sharing.
- Namespace lifecycle management (creation/deletion via API).
- Namespace persistence (the snapshot file captures one namespace's state).
- Migrating existing single-connection primitives to namespaces.

## Technical Design

### Namespace Registry

```go
type Namespace struct {
    name       string
    primitives sync.Map           // map[id]primEntry
    mu         sync.RWMutex       // for create/delete atomicity
    connCount  atomic.Int32
}

type NamespaceRegistry struct {
    namespaces sync.Map           // map[namespace_name]*Namespace
}

func (r *NamespaceRegistry) GetOrCreate(name string) *Namespace {
    actual, _ := r.namespaces.LoadOrStore(name, &Namespace{name: name})
    return actual.(*Namespace)
}
```

### Namespace Selection

WebSocket clients specify their namespace via:
1. When JWT auth is enabled: `namespace` claim in the JWT payload.
2. When auth is disabled: `?ns=<name>` query parameter (for development/testing).
3. Default namespace: `"default"` (when no namespace is specified).

```go
func extractNamespace(r *http.Request, claims *auth.Claims) string {
    if claims != nil && claims.Namespace != "" {
        return claims.Namespace
    }
    if ns := r.URL.Query().Get("ns"); ns != "" {
        return ns
    }
    return "default"
}
```

### Connection Assignment

In `HandleWebSocket`:
```go
namespace := extractNamespace(r, claims)
ns := s.nsRegistry.GetOrCreate(namespace)
ns.connCount.Add(1)
defer ns.connCount.Add(-1)
// All primitive operations go through ns.primitives instead of a local map
```

### Primitive Operations with Namespacing

Replace the per-connection primitive map with `ns.primitives`:
```go
// Before:
primitives := make(map[string]primEntry)
primitives[id] = primEntry{ctx: ctx, cancel: cancel}

// After:
ns.primitives.Store(id, primEntry{ctx: ctx, cancel: cancel})
entry, ok := ns.primitives.Load(id)
```

### Scheduler Integration

The `Scheduler` receives a `namespace` field on all primitive registrations:
```go
s.scheduler.RegisterPrimitive(id, ptype, name, stats)
// becomes:
s.scheduler.RegisterPrimitive(namespace+"/"+id, ptype, name, stats)
```

This ensures the scheduler's primitive map uses namespace-scoped IDs.

## Backend Implementation

1. Add `Namespace` and `NamespaceRegistry` types to a new `internal/namespace/namespace.go`.
2. Add `nsRegistry *NamespaceRegistry` to `*Server`.
3. Initialize `nsRegistry` in `NewServerWithConfig`.
4. Update `HandleWebSocket` to extract namespace and bind to `Namespace`.
5. Replace the per-connection `map[string]primEntry` with `ns.primitives` (`sync.Map`).
6. Update all primitive create/delete/op handlers to use the namespace's primitive store.
7. Update `extractNamespace` to read from JWT claims (after TICKET-016).
8. Add `Config.DefaultNamespace` (default: `"default"`).
9. Add tests for namespace isolation and sharing.

## Frontend Implementation

The dashboard must allow selecting/creating namespaces:
1. Add a namespace selector dropdown in the header.
2. When connecting, include `?ns=<name>` in the WebSocket URL (for unauthenticated mode).
3. The primitive list shows primitives in the selected namespace only.

## Database / State Changes

The snapshot file captures primitives for a single namespace. Add `namespace` to the snapshot envelope:
```json
{"version": 1, "namespace": "production", "primitives": {...}}
```

## API Changes

- New optional `?ns=<namespace>` query parameter on `/ws` (when auth is disabled).
- JWT `namespace` claim used when auth is enabled.
- All primitive IDs in the scheduler are now namespace-scoped: `namespace/id`.
- `primitiveOp`, `createMutex`, etc.: the `id` field is still just the primitive ID (not namespace-prefixed from the client's perspective). The server prepends the namespace internally.

## Infrastructure Requirements

None.

## Edge Cases

- Multiple connections in the same namespace operating on the same primitive simultaneously: the namespace's `sync.Map` and each primitive's internal atomics handle concurrent access.
- Namespace name length: apply the same 256-char limit as primitive IDs.
- Namespace name with `/` or other special characters: sanitize or reject.
- Empty namespace `""`: treat as `"default"`.
- Connection disconnect while a goroutine is blocked on a namespace-shared primitive: TICKET-010 (context cancellation) handles this — the goroutine exits, but the primitive is not deleted (it belongs to the namespace, not the connection).

## Failure Handling

- Namespace creation failure: `sync.Map.LoadOrStore` is always safe. No allocation failure.
- Namespace not found (if explicit namespace management is added later): return 403.

## Security Considerations

- Without authentication, any client can join any namespace via `?ns=`. This is acceptable for development but must be disabled in production (require JWT with namespace claim).
- Namespace names should be validated to prevent namespace hijacking (e.g., a client claiming namespace `admin`).
- Document: "In production, always use JWT authentication with explicit namespace claims."

## Testing Plan

### Unit Tests

```go
func TestNamespaceIsolation(t *testing.T) {
    // Connect clientA to namespace "ns-a"
    // Connect clientB to namespace "ns-b"
    // clientA creates mutex "lock1"
    // clientB tries to use "lock1" — should get "primitive not found"
}

func TestNamespaceSharing(t *testing.T) {
    // Connect clientA and clientB to namespace "shared"
    // clientA creates barrier with parties=2
    // clientA calls barrier.wait
    // clientB calls barrier.wait
    // Both should unblock (barrier tripped with 2 parties)
}

func TestDefaultNamespace(t *testing.T) {
    // Connect without ?ns= parameter
    // Assert joined "default" namespace
}
```

### Integration Tests

Run `TestWebSocketLoad` with namespace support. Verify each connection in the load test uses a unique namespace to avoid interference.

### E2E Tests

Manual: open two browser tabs, both connected to namespace `test-ns`. Create a mutex in tab A. Verify tab B sees the mutex and can operate on it.

## Monitoring Requirements

- `syncprim_active_namespaces` gauge — number of namespaces with at least one connection.
- `syncprim_connections_per_namespace` histogram — distribution of connections per namespace.

## Logging Requirements

```
level=INFO msg="connection joined namespace" conn_id="c1" namespace="production"
level=INFO msg="namespace created" namespace="production"
level=INFO msg="namespace destroyed (no remaining connections)" namespace="test-ns"
```

## Metrics to Track

- `syncprim_active_namespaces` — gauge

## Rollback Plan

This is a significant architectural change. Rollback path:
1. Revert `HandleWebSocket` to use per-connection primitive map.
2. Remove `NamespaceRegistry` and `Namespace` types.
3. Clients in the `"default"` namespace will have their primitives isolated again.
4. Data loss: any primitives in shared namespaces are lost (they were in memory only).

## Acceptance Criteria

- [ ] Connections in different namespaces cannot see each other's primitives
- [ ] Connections in the same namespace share primitive state
- [ ] Default namespace `"default"` is used when no namespace is specified
- [ ] Namespace is included in snapshot persistence
- [ ] `TestNamespaceIsolation` and `TestNamespaceSharing` pass
- [ ] JWT namespace claim respected (after TICKET-016)

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] Coverage ≥70%
- [ ] Architecture diagram updated in README
- [ ] CHANGELOG entry (breaking change for existing single-connection users)
