# TICKET-006: Add gosec Static Security Analysis to CI Pipeline

**Type:** infra
**Priority:** P1
**Estimate:** S (1 day)
**Epic:** Observability and Reliability
**Labels:** p1, sprint-2, ci, security, devops
**Status:** TODO

## Problem Statement

The current CI pipeline (`ci.yml`) runs `go build`, `go vet`, race-detector tests, and benchmarks. It does not include any security-focused static analysis. `go vet` checks for code correctness issues but not security patterns.

`gosec` (Go Security Checker) performs AST-based analysis to detect:
- G101: Hardcoded credentials
- G102: Binding to all network interfaces (0.0.0.0 without a comment)
- G104: Errors unhandled (`errcheck` equivalent)
- G107: URL construction from user input (SSRF candidates)
- G112: Insecure TLS minimum version
- G201/G202: SQL injection patterns
- G304: File path handling (path traversal)
- G401–G501: Weak cryptographic algorithms
- G601: Implicit memory aliasing in for loops (fixed in Go 1.22+)

Several of these are directly relevant to this codebase:
- G304 is relevant to `filepath.Abs` usage in the static file handler
- G107 is relevant to WebSocket URL handling
- G104 is relevant to deferred `conn.Close()` and `recover()` paths
- G112 is relevant to the TLS configuration

## Context

Current `.github/workflows/ci.yml` jobs:
- `build-test`: build, vet, race test, coverage, examples
- `bench`: benchmarks

`gosec` is not present. The team has no automated security analysis gate.

## Goals

1. Add a `security` job to `.github/workflows/ci.yml` that runs `gosec`.
2. Use `gosec -severity medium -confidence medium` (do not fail on low-severity, low-confidence findings to avoid noise).
3. Fix all issues identified by `gosec` in the current codebase.
4. Add `gosec` to the `Makefile` as a `security` target.
5. Document the `make security` command in CONTRIBUTING.md.

## Non-Goals

- Running `gosec` in pre-commit hooks (can be added later by individual developers).
- Running other security scanners (Semgrep, CodeQL) in this ticket.
- Fixing issues in test files (apply `//nolint:gosec` with a comment explaining why).

## Technical Design

Add to `.github/workflows/ci.yml`:

```yaml
security:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4

    - name: Set up Go
      uses: actions/setup-go@v5
      with:
        go-version: "1.21"

    - name: Install gosec
      run: go install github.com/securego/gosec/v2/cmd/gosec@v2.21.0

    - name: Run gosec
      run: gosec -severity medium -confidence medium -exclude-generated ./...
```

Add to `Makefile`:
```makefile
security:
	@which gosec > /dev/null || go install github.com/securego/gosec/v2/cmd/gosec@v2.21.0
	gosec -severity medium -confidence medium ./...
```

## Backend Implementation

After adding the CI step, run `gosec ./...` locally and fix all findings:

Expected findings and resolutions:

**G304 (File path from user input):**
```go
// In web/server.go HandleStatic:
// gosec flags: filepath.Join(root, r.URL.Path)
// Fix: already uses filepath.Abs + strings.HasPrefix guard
// Add: //nolint:gosec // path traversal prevented by Abs+HasPrefix guard
```

**G104 (Errors unhandled):**
```go
// gorilla/websocket conn.Close() errors in deferred closes
// Fix: capture and log: if err := conn.Close(); err != nil { slog.Debug(...) }
```

**G112 (Insecure TLS minimum version):**
```go
// If TLS config does not set MinVersion, gosec flags it
// Fix: set MinVersion: tls.VersionTLS12 in the tls.Config
```

For findings that are false positives or cannot be fixed without changing behavior, add `//nolint:gosec` with a comment explaining why.

## Frontend Implementation

None.

## Database / State Changes

None.

## API Changes

None (TLS minimum version change is transparent to clients that support TLS 1.2+).

## Infrastructure Requirements

`gosec` binary must be installed in the CI environment. The `go install` command handles this.

## Edge Cases

- `gosec` may flag the `unsafe` package usage (if any). There is currently no `unsafe` usage in this codebase.
- `gosec` may flag `rand.Int()` without a seed (not relevant here as no `math/rand` is used for security-sensitive operations).
- Some findings in vendor or test code should be suppressed with `-exclude-dir=vendor` and not `-exclude-generated` alone.

## Failure Handling

If `gosec` reports findings at medium severity or higher:
1. The CI job fails.
2. The developer receives a list of findings in the job output.
3. Findings must be fixed or explicitly suppressed with a `//nolint:gosec` comment explaining why before the PR can merge.

## Security Considerations

`gosec` itself is a security tool. Pinning it to a specific version (`@v2.21.0`) prevents supply chain attacks via a compromised `@latest` tag. Use `go install ... @v2.21.0` (specific version) not `@latest`.

## Testing Plan

### Unit Tests

None required for a CI tooling change.

### Integration Tests

The CI job itself serves as the integration test. Verify it passes on the main branch after fixing all findings.

### E2E Tests

Manual: run `make security` locally, confirm it exits 0 after fixes are applied. Introduce a deliberate issue (e.g., `//nolint:gosec` removal on a suppressed finding) and confirm `make security` exits non-zero.

## Monitoring Requirements

CI job status is visible in GitHub Actions. No server-side metrics changes.

## Logging Requirements

`gosec` output is standard and self-documenting. No additional logging needed.

## Metrics to Track

Track the number of gosec suppressions (`//nolint:gosec` comments) over time as a proxy for the security debt backlog. Consider a future automation to alert if suppression count increases unexpectedly.

## Rollback Plan

Remove the `security` job from `ci.yml`. The Makefile target can remain for local use. No production impact.

## Acceptance Criteria

- [ ] `security` job added to `.github/workflows/ci.yml`
- [ ] `gosec` pinned to a specific version (not `@latest`)
- [ ] `make security` target added to `Makefile`
- [ ] All medium-or-higher severity gosec findings fixed or suppressed with comments
- [ ] CI pipeline is green on the main branch with all three jobs: `build-test`, `bench`, `security`
- [ ] TLS MinVersion set to `tls.VersionTLS12` if not already
- [ ] CONTRIBUTING.md mentions `make security`

## Definition of Done

- [ ] Code reviewed and merged
- [ ] CI green
- [ ] No new `//nolint:gosec` suppressions without explanatory comments
- [ ] Documentation updated (CONTRIBUTING.md)
