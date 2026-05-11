# TICKET-019: WebSocket permessage-deflate Compression

**Type:** improvement
**Priority:** P2
**Estimate:** S (1–2 days)
**Epic:** Scalability and Performance
**Labels:** p2, sprint-13, performance, websocket, compression
**Status:** SHIPPED

**Tracking Note:** Implemented on `origin/main`. This ticket is retained as historical planning context; the design notes and checklists below may not match the final shipped implementation verbatim.


## Problem Statement

WebSocket `update` messages containing full primitive state can be 50+ KB each. The server broadcasts at 10 Hz (every 100 ms) to all connected clients. Without compression, this is significant bandwidth — especially for clients on slow or metered connections, or when running many concurrent connections in load tests.

The `permessage-deflate` WebSocket extension (RFC 7692) enables per-message deflate compression, which can reduce message sizes by 60–80% for typical JSON payloads due to repetitive structure.

`github.com/gorilla/websocket` supports `permessage-deflate` via the `EnableCompression: true` flag on the `Upgrader`.

## Context

Current upgrader configuration in `web/server.go`:
```go
upgrader := websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool {
        return s.checkOrigin(r)
    },
    ReadBufferSize:  4096,
    WriteBufferSize: 4096,
}
```

No `EnableCompression` field is set. Compression is disabled.

## Goals

1. Enable `EnableCompression: true` on the WebSocket upgrader.
2. Verify compatibility with gorilla/websocket's compression implementation.
3. Verify browser compatibility (all major browsers support permessage-deflate).
4. Add a `CompressionEnabled` field to `Config` (default `true` but can be disabled for debugging).
5. Measure and document bandwidth reduction in the benchmark.

## Non-Goals

- Implementing custom compression algorithms.
- Compressing the HTTP REST responses (`/metrics`, `/healthz`).
- Controlling per-client compression context (use server-level default).

## Technical Design

```go
upgrader := websocket.Upgrader{
    CheckOrigin: func(r *http.Request) bool {
        return s.checkOrigin(r)
    },
    ReadBufferSize:  4096,
    WriteBufferSize: 4096,
    EnableCompression: s.cfg.CompressionEnabled, // default: true
}
```

Add to `Config`:
```go
// CompressionEnabled enables permessage-deflate WebSocket compression.
// Default: true. Disable for debugging or compatibility issues.
CompressionEnabled bool
```

Note: Go's zero value for `bool` is `false`, which would disable compression by default. Use a `DisableCompression bool` field (inverted semantics) or initialize `CompressionEnabled = true` explicitly in `NewServerWithConfig` when the field is its zero value:

```go
if !cfg.compressionExplicitlySet {
    cfg.CompressionEnabled = true
}
```

Alternatively, use a pointer: `CompressionEnabled *bool` where `nil` means default (true).

Simplest approach: make `DisableCompression bool` — compression is on by default, set to `true` to disable.

## Backend Implementation

1. Add `DisableCompression bool` to `Config`.
2. In `NewServerWithConfig`, construct the upgrader with:
   ```go
   EnableCompression: !cfg.DisableCompression
   ```
3. Add `--disable-compression` CLI flag to `cmd/server/main.go`.
4. Add `TestWebSocketCompressionEnabled` that verifies compressed messages are smaller than uncompressed.
5. Benchmark: `BenchmarkWebSocketUpdateCompressed` vs `BenchmarkWebSocketUpdateUncompressed`.

## Frontend Implementation

The browser WebSocket API automatically negotiates compression extensions during the HTTP upgrade handshake. No JavaScript changes are needed. The browser transparently decompresses received messages.

## Database / State Changes

None.

## API Changes

The WebSocket handshake will include the `Sec-WebSocket-Extensions: permessage-deflate` header when compression is enabled. This is standard and expected by all browsers.

## Infrastructure Requirements

- Compression adds CPU overhead on both server and client.
- The gorilla/websocket library uses a shared `flate.Writer` per connection (allocated from a pool). Memory overhead is approximately 32 KB per connection for the deflate dictionary.
- With 1000 connections: 32 MB overhead. Acceptable within the 512 Mi memory limit.

## Edge Cases

- Clients that do not support permessage-deflate: the extension is negotiated during the handshake. If the client does not offer the extension, the server does not use it for that connection. No compatibility issue.
- Very small messages (<100 bytes): compression overhead exceeds savings. gorilla/websocket handles this gracefully; small messages may be sent uncompressed even when compression is enabled.
- Long-running connections: the deflate dictionary accumulates state over the connection lifetime. This improves compression ratios for repetitive messages (like our recurring `update` messages with similar structure).

## Failure Handling

If compression negotiation fails during the upgrade, gorilla/websocket falls back to uncompressed. This is transparent.

## Security Considerations

- **CRIME/BREACH attack**: These attacks exploit compression + encryption to extract secrets from compressed ciphertext. They require the attacker to control part of the plaintext and observe compressed ciphertext lengths. In this application:
  - Messages contain primitive state (IDs, names, counters) — not secrets.
  - API keys and JWTs are in HTTP headers (not in the WebSocket payload body).
  - The attack surface is minimal.
- For maximum security, disable compression when TLS is enabled and sensitive data might appear in messages (paranoid mode). Add a note to the documentation.

## Testing Plan

### Unit Tests

```go
func TestCompressionReducesMessageSize(t *testing.T) {
    // Create server with CompressionEnabled=true (default)
    // Create server without compression
    // Create many primitives to make update messages large
    // Capture update messages from both servers
    // Assert compressed messages are smaller
    // (Note: this requires capturing raw WebSocket frames, which is tricky.
    //  Alternative: measure with network traffic analysis or http transport metrics.)
}
```

A simpler approach: measure the size of the `update` payload before and after compression by serializing to JSON and using the `compress/flate` package directly in the test.

### Integration Tests

Run `TestWebSocketLoad` with compression enabled. Verify no regressions.

### E2E Tests

Manual: use browser devtools Network tab to compare WebSocket frame sizes with and without compression. Create 50 primitives, observe the `update` frame sizes.

## Monitoring Requirements

No new metrics. Monitor memory usage and CPU usage in production to verify compression overhead is within expected bounds.

## Logging Requirements

```
level=INFO msg="WebSocket compression enabled"
level=DEBUG msg="WebSocket compression disabled for connection (client does not support)" remote_addr="..."
```

## Metrics to Track

None new. Monitor `process_resident_memory_bytes` to verify the 32 KB/connection overhead is within budget.

## Rollback Plan

Set `DisableCompression: true` in Config. No restart needed if configurable at runtime; server restart required otherwise. No data loss.

## Acceptance Criteria

- [ ] `EnableCompression: true` set on the upgrader by default
- [ ] `Config.DisableCompression` flag and `--disable-compression` CLI flag work
- [ ] Browser connects successfully with compression negotiated (verified via devtools)
- [ ] `TestWebSocketLoad` passes with compression enabled
- [ ] No memory leaks (goroutine count stable under load with compression)

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Tests passing with race detector
- [ ] README configuration table updated
- [ ] CHANGELOG entry written
