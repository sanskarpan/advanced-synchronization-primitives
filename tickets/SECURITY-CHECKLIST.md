# Security Validation Checklist

**Project:** Advanced Synchronization Primitives
**Version:** v0.2.0
**Updated:** 2026-05-09

This checklist must be completed before every release by a security-aware reviewer. Failures must be filed as security tickets (use responsible disclosure if the issue is exploitable in production).

---

## 1. Authentication

- [ ] API key authentication works: `Authorization: Bearer <key>` is required when `--api-key` is set
- [ ] `?key=<apikey>` query parameter is rejected with HTTP 401 (TICKET-003 complete)
- [ ] Empty API key (`--api-key ""`) disables authentication entirely — verify this is intended for dev environments only
- [ ] API key comparison uses `subtle.ConstantTimeCompare` to prevent timing attacks
- [ ] API key is never logged at any log level (grep for `cfg.APIKey` in log calls — must be zero)
- [ ] HTTPS is required when `--api-key` is set in production (document this requirement)

### JWT Authentication (after TICKET-016)
- [ ] Expired JWT is rejected with HTTP 401
- [ ] JWT with invalid signature is rejected with HTTP 401
- [ ] JWT with missing `exp` claim is rejected
- [ ] JWT with missing `sub` claim is rejected
- [ ] JWT signature is verified with HMAC-SHA256 using `hmac.Equal` (constant-time)
- [ ] JWT value is never logged at any log level
- [ ] JWT secret is at least 32 bytes (256 bits) — warn if shorter

---

## 2. Input Validation

- [ ] `id` field longer than 256 characters is rejected (TICKET-001 complete)
- [ ] `name` field longer than 256 characters is rejected (TICKET-001 complete)
- [ ] `holdMs` exceeding 3,600,000 ms is clamped with warning, not silently accepted without notification (TICKET-002 complete)
- [ ] `capacity` ≤ 0 is rejected with error (existing validation in primitives layer)
- [ ] `parties` ≤ 0 is rejected with error (existing validation in primitives layer)
- [ ] Integer overflow not possible: `holdMs` is an `int` (32-bit min on platforms where int is 32-bit); verify JSON deserialization does not overflow
- [ ] `op` field of unknown value is handled gracefully (returns error, not panic)
- [ ] All validation errors return informative error responses, not panic/500

---

## 3. Origin Control (CORS)

- [ ] When `--allowed-origins` is empty (default), only `localhost` origins are accepted
- [ ] Request from a non-allowed origin is rejected with HTTP 403 during WebSocket upgrade
- [ ] `--allowed-origins *` accepts all origins (document this as development-only)
- [ ] Origin header cannot be forged by JavaScript in a browser (true for HTTP headers in browser WS connections; verify behavior)

---

## 4. TLS Configuration

- [ ] `--tls-cert` and `--tls-key` both required to enable TLS (either alone returns an error)
- [ ] Invalid cert/key path causes server to fail to start with a clear error (not panic)
- [ ] TLS minimum version is TLS 1.2 (`tls.VersionTLS12`) (after TICKET-006 gosec scan)
- [ ] Self-signed certificates work for development testing

---

## 5. Resource Exhaustion / DoS

- [ ] Connection cap (`MaxConns`) enforced: 1001st connection gets HTTP 503
- [ ] Per-connection rate limit enforced: 201st message/second gets error
- [ ] Per-connection op rate limit enforced: 51st op/second gets error (after TICKET-008)
- [ ] WebSocket message size cap enforced: messages > 64 KiB cause connection to close
- [ ] Goroutine cap in scheduler: more than 10,000 goroutines registered causes silently dropped registrations (not panic)
- [ ] Long `holdMs` (1 hour) does not permanently hold a primitive after connection closes (TICKET-010 ensures context cancellation)
- [ ] Empty `id` does not cause nil pointer panic in any handler
- [ ] Oversized snapshot file (e.g., manually crafted 100 MB JSON) does not crash the server on startup (tested with `os.ReadFile` which returns an error on very large files only if memory is insufficient — document this limitation)

---

## 6. Path Traversal

- [ ] Static file handler rejects paths with `..` sequences
- [ ] `filepath.Abs` + `strings.HasPrefix` guard is in place for static files
- [ ] Request for `/../../etc/passwd` returns 404 or 400, not file contents
- [ ] Request for `/` (root) returns the dashboard, not the directory listing
- [ ] No other file-based operations are exposed via the HTTP or WebSocket API

---

## 7. XSS (Cross-Site Scripting)

- [ ] All user-supplied strings in the dashboard (primitive IDs, names) are rendered as text nodes, not via `innerHTML`
- [ ] Primitive names containing `<script>alert('xss')</script>` appear as literal text in the dashboard
- [ ] Toast error messages containing HTML are escaped (rendered as text, not HTML)
- [ ] No `eval()` or `Function()` calls in the dashboard JavaScript
- [ ] Content-Security-Policy header set on HTML response (future improvement, document current gap)

---

## 8. Injection

- [ ] Primitive IDs and names are not used in any SQL queries (no SQL in this project — confirm)
- [ ] Primitive IDs and names are not interpolated into OS commands (no `exec.Command` with user input — confirm)
- [ ] Snapshot file path (`Config.SnapshotPath`) is set by the operator (not user input); verify no WebSocket message can override it

---

## 9. Information Disclosure

- [ ] Error responses do not include internal stack traces (only the error message)
- [ ] The `recover()` defer in `handlePrimitiveOp` logs the stack trace server-side only, not sent to the client
- [ ] `/healthz` response does not include server version or Go version (could reveal vulnerabilities)
- [ ] `/metrics` response does not include API keys or JWT secrets in label values
- [ ] WebSocket error messages do not include file paths, internal goroutine IDs, or server-side variable values

---

## 10. Dependency Security

- [ ] Run `go mod verify` — all modules match expected checksums
- [ ] Run `govulncheck ./...` — no known vulnerabilities in dependencies
  ```bash
  go install golang.org/x/vuln/cmd/govulncheck@latest
  govulncheck ./...
  ```
- [ ] `github.com/gorilla/websocket` is the latest stable version (check for CVEs)
- [ ] Go toolchain is up to date (check https://go.dev/doc/security/vuln/)

---

## 11. gosec Scan (after TICKET-006)

- [ ] `make security` exits 0 (no medium-or-higher severity findings)
- [ ] All `//nolint:gosec` suppressions have explanatory comments
- [ ] No hardcoded credentials in source code
- [ ] No `G304` (file path from user input) findings without explicit justification

---

## 12. Docker Security

- [ ] Container runs as non-root user (`appuser` with UID/GID from Alpine `adduser`)
- [ ] Container does not run with `--privileged` flag
- [ ] Base image (`alpine:3.19`) is not known to have critical CVEs (check `docker scout` or `trivy`)
- [ ] Multi-stage build: only the compiled binary is in the runtime image (no Go toolchain)
- [ ] No secrets baked into the Docker image (check `docker history`)

---

## 13. Kubernetes Security (after TICKET-015)

- [ ] `readOnlyRootFilesystem: true` in container securityContext
- [ ] `allowPrivilegeEscalation: false` set
- [ ] `capabilities: drop: ["ALL"]` set
- [ ] API key stored in Kubernetes Secret, not ConfigMap
- [ ] Network policy restricts access to the WebSocket port (document recommended policy)

---

## 14. Audit Log Security (after TICKET-017)

- [ ] Audit log file has permissions `0640` (owner r/w, group r, others none)
- [ ] Audit log does not contain JWT tokens or API keys
- [ ] Audit log directory has permissions `0750`

---

## 15. Regression Tests for Security Fixes

- [ ] `TestWebSocketAuthQueryParamRejected` passes (TICKET-003)
- [ ] `TestValidationIDTooLong` passes (TICKET-001)
- [ ] `TestValidationNameTooLong` passes (TICKET-001)
- [ ] `TestHTTPServerTimeouts` passes (connection with no data should timeout)
- [ ] Static file path traversal test passes (`TestStaticFilePathTraversal` in `web/server_test.go`)

---

## Sign-off

Security review must be signed off by at least one engineer with security experience before any public release.

| Section | Reviewer | Date | Status | Notes |
|---------|----------|------|--------|-------|
| 1. Authentication | | | | |
| 2. Input Validation | | | | |
| 3. Origin Control | | | | |
| 4. TLS | | | | |
| 5. DoS | | | | |
| 6. Path Traversal | | | | |
| 7. XSS | | | | |
| 8. Injection | | | | |
| 9. Info Disclosure | | | | |
| 10. Dependencies | | | | |
| 11. gosec | | | | |
| 12. Docker | | | | |

**Overall Security Status:** [ ] PASS  [ ] FAIL (open issues listed below)

**Open Issues:**
(List any failed checks here with severity and ticket reference)
