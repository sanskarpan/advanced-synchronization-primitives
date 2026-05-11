# Contributing to Advanced Synchronization Primitives

Thank you for taking the time to contribute. This document explains everything you need to get productive: environment setup, test commands, branch and commit conventions, the PR process, and how to report security vulnerabilities.

---

## Code of Conduct

This project follows the [Contributor Covenant Code of Conduct v2.1](https://www.contributor-covenant.org/version/2/1/code_of_conduct/). By participating you agree to uphold these standards. Report unacceptable behavior to the repository maintainers.

---

## Setting Up Your Local Environment

### Prerequisites

| Tool | Minimum version | Install |
|------|----------------|---------|
| Go | 1.21 | https://golang.org/dl/ |
| Git | 2.38 | https://git-scm.com/ |
| Make | 3.81 | pre-installed on Linux/macOS |
| Docker | 24.0 (optional) | https://docs.docker.com/get-docker/ |

### Fork and Clone

```bash
# 1. Fork on GitHub, then:
git clone https://github.com/<YOUR_USERNAME>/advanced-synchronization-primitives.git
cd advanced-synchronization-primitives

# 2. Add the upstream remote so you can stay in sync
git remote add upstream https://github.com/sanskarpan/advanced-synchronization-primitives.git
git fetch upstream
```

### Install Dependencies

```bash
go mod download
go mod verify
```

There is intentionally only one external dependency: `github.com/gorilla/websocket`. All other functionality is implemented in the standard library or in this repository.

### Verify Your Setup

```bash
make build     # should produce ./syncprimitives-server
make test      # all tests should pass
make race      # race detector run (takes ~60 s)
make coverage  # should report ≥70%
```

---

## Running Tests

### All Tests

```bash
make test
# equivalent: go test -timeout 120s ./...
```

### With Race Detector

The race detector is **mandatory** before submitting a PR. The CI pipeline runs it on every push.

```bash
make race
# equivalent: go test -race -count=3 -timeout 120s ./...
```

`-count=3` runs each test three times to increase the probability of catching races under scheduler variation.

### Coverage Report

```bash
make coverage
# Generates coverage.out and fails if total coverage < 70%
```

To browse per-function coverage in a browser:

```bash
go tool cover -html=coverage.out
```

### Benchmarks

```bash
make bench
# equivalent: go test -bench=. -benchmem -timeout 120s ./internal/primitives/
```

On pull requests, CI also runs benchmark comparison against the latest `main`
baseline using `benchstat`. Regressions greater than 20% in `ns/op` fail the
benchmark-comparison gate.

To compare benchmarks before and after your change:

```bash
# Before your change (on main):
go test -bench=. -benchmem ./internal/primitives/ > before.txt

# After your change:
go test -bench=. -benchmem ./internal/primitives/ > after.txt

# Compare (requires golang.org/x/perf/cmd/benchstat):
go install golang.org/x/perf/cmd/benchstat@latest
benchstat before.txt after.txt
```

### Fuzz Tests

```bash
# Run the semaphore fuzz target for 30 seconds
go test -fuzz=FuzzSemaphoreAcquireRelease -fuzztime=30s ./internal/primitives/

# Run the barrier fuzz target
go test -fuzz=FuzzBarrierWait -fuzztime=30s ./internal/primitives/
```

If the fuzzer finds a crash, a seed corpus entry is written to `testdata/fuzz/`. Commit that file so the crash is reproduced in CI.

### Lint

```bash
make lint
# equivalent: go vet ./...
```

For extended linting (optional but recommended):

```bash
# Install staticcheck
go install honnef.co/go/tools/cmd/staticcheck@latest
staticcheck ./...
```

---

## Project Layout

```
internal/primitives/   — all 8 primitives; unit tests in *_test.go files
internal/metrics/      — MetricsCollector
internal/scheduler/    — Scheduler (primitive registry, events)
internal/loadtest/     — WebSocket load tests (skipped with -short)
web/                   — HTTP server, WebSocket handler, static assets
cmd/server/            — binary entry point
examples/              — stand-alone example programs
```

Keep primitives in `internal/` — they are not part of the public API surface yet. Public API will be stabilized in a future release.

---

## Branch Naming Conventions

Branches must follow this format: `<type>/<short-description>`

| Prefix | Use for |
|--------|---------|
| `feat/` | New features (new primitive variant, new endpoint, new flag) |
| `fix/` | Bug fixes (correctness issues, crashes, wrong return values) |
| `test/` | Adding or fixing tests without changing production code |
| `docs/` | Documentation-only changes |
| `chore/` | Tooling, CI, dependency updates, Makefile changes |
| `perf/` | Performance improvements (no semantic change) |
| `refactor/` | Code restructuring that does not change external behavior |
| `security/` | Security fixes |

Examples:
```
feat/fair-rwlock-variant
fix/condvar-waitttimeout-elapsed
test/barrier-over-subscription
docs/singleflight-forget-docs
chore/add-gosec-ci
security/remove-api-key-url-param
```

---

## Commit Message Format

This project uses [Conventional Commits](https://www.conventionalcommits.org/).

```
<type>(<optional scope>): <imperative summary, ≤72 chars>

[optional body: wrap at 72 chars, explain WHY not WHAT]

[optional footer: Fixes #NNN, BREAKING CHANGE: ...]
```

### Types

| Type | When to use |
|------|-------------|
| `feat` | Introduces a new feature |
| `fix` | Fixes a bug |
| `test` | Adds or modifies tests only |
| `docs` | Documentation only |
| `chore` | Build, CI, dependency, tooling |
| `perf` | Performance improvement |
| `refactor` | Code change that neither fixes a bug nor adds a feature |
| `security` | Security fix |

### Scope (optional)

Use the package or layer name: `primitives`, `metrics`, `scheduler`, `web`, `server`, `ci`, `docker`.

### Examples

```
feat(primitives): add Fair RWLock variant with FIFO ordering

The current RWLock is writer-preference which can starve readers under
sustained write load. This variant uses a single FIFO queue for both
readers and writers, granting acquisition in arrival order.

Fixes #42

fix(web): reject API key from URL query parameter

Passing secrets in URLs leaks them into server logs, browser history,
and reverse-proxy access logs. Clients must now use Authorization:
Bearer <key> exclusively.

BREAKING CHANGE: ?key=<apikey> query parameter no longer accepted.

test(primitives): add lost-wakeup regression tests for Semaphore

Adds three deterministic tests that reproduce the window between
count-check and Enqueue by injecting controlled goroutine scheduling
delays via runtime.Gosched().
```

---

## Pull Request Process

### Before You Open a PR

1. Rebase your branch on the latest `main`:
   ```bash
   git fetch upstream
   git rebase upstream/main
   ```

2. Run the full validation suite:
   ```bash
   make race      # must pass — zero races
   make coverage  # must be ≥70%
   make lint      # must produce no output
   make build     # must succeed
   ```

3. If you added a new primitive or endpoint, add a usage example to the relevant `examples/` directory or update the README.

4. Update `CHANGELOG.md` with a brief entry under an `## Unreleased` heading.

### PR Title and Description

- Title must follow the Conventional Commits format (same as commit messages).
- Description must include:
  - **What** changed and **why**
  - **Testing evidence**: which tests were added or modified
  - **Benchmark delta** if the change touches a hot path

### Review Criteria

Reviewers will check:

1. **Correctness**: Does the implementation match the documented semantics? Are there lost-wakeup windows? Race conditions?
2. **Race detector clean**: The CI race run must be green.
3. **Coverage**: New code must be covered. Overall coverage must not drop below 70%.
4. **Atomics discipline**: All shared state accessed through `atomic` types or under a lock. No plain reads of `int32`/`int64` fields modified from other goroutines.
5. **Error handling**: No swallowed errors. Use `fmt.Errorf("context: %w", err)` for wrapping.
6. **Panic vs error**: Programmer errors (wrong argument types, invalid state) may panic. Runtime errors (resource exhaustion, timeout) must return errors.
7. **API compatibility**: Changes to exported types require a discussion in the issue first.
8. **Documentation**: Exported types and functions must have godoc comments.

### Review Response Time

Maintainers aim to provide first-pass review within 5 business days. If your PR has been open for more than 7 days without a response, ping in the PR comments.

---

## Testing Requirements for PRs

| Requirement | Detail |
|-------------|--------|
| Race detector | `go test -race ./...` must pass with zero data race reports |
| Coverage gate | Total coverage ≥ 70% (enforced by `make coverage` in CI) |
| New code coverage | Every new function or method must have at least one direct test |
| Fuzz corpus | If fixing a parser or state machine bug, add a seed to the relevant fuzz corpus |
| Benchmark regression | For hot-path changes, include `benchstat` output showing no regression (>20% slowdown fails CI unless baseline is unavailable) |
| Race test iterations | CI runs `-count=3`; locally you may run `-count=1` but must run `-count=3` before final submission |

### Writing Tests for Synchronization Code

Synchronization primitives require specialized testing techniques:

1. **Use `testing/synctest` or goroutine sequencing**: Control goroutine scheduling with channels to create deterministic interleavings.
2. **Test the lost-wakeup window explicitly**: Use `runtime.Gosched()` between steps to stress-test CAS-then-enqueue sequences.
3. **Always test with `-race`**: Even if a test does not explicitly exercise concurrency, the race detector can flag benign-looking code.
4. **Test context cancellation**: Every `*Context` method must have a test that cancels the context before the blocking condition is satisfied.
5. **Test timeout accuracy**: `WaitTimeout` tests must verify that the function returns within a reasonable window (e.g., `timeout + 50ms`) — not that it returns in exactly `timeout`.

---

## Security Vulnerability Reporting

**Do not open a public GitHub issue for security vulnerabilities.**

To report a vulnerability:

1. Send an email to the repository owner with the subject line: `[SECURITY] Advanced Synchronization Primitives — <brief description>`
2. Include:
   - A description of the vulnerability and its impact
   - Steps to reproduce (minimal reproducer preferred)
   - Go version and OS you tested on
   - Whether you have a proposed fix
3. You will receive an acknowledgment within 48 hours.
4. We aim to release a fix within 14 days of confirmation, depending on severity and complexity.
5. You will be credited in the security advisory and CHANGELOG unless you prefer to remain anonymous.

### Scope

The following are in scope for security reports:

- Authentication bypass in WebSocket API key handling
- Information disclosure via error messages or metrics endpoints
- Denial of service via unbounded resource consumption (goroutine leak, memory exhaustion)
- Injection vulnerabilities in the dashboard (XSS)
- TLS misconfiguration

The following are out of scope:

- Vulnerabilities in the underlying Go standard library (report to https://go.dev/security)
- Vulnerabilities in gorilla/websocket (report upstream)
- Issues in example programs only (not the library or server)

---

## Style Guide

- Follow standard Go formatting: `gofmt` / `goimports`
- Use `log/slog` for all new logging — no `fmt.Println`, `log.Printf`, or bare `fmt.Fprintf(os.Stderr, ...)` in production code paths
- Prefer `atomic.Int64` / `atomic.Int32` over `sync/atomic.LoadInt64` for new code (Go 1.19+ typed atomics)
- Comment every exported symbol; comment every unexported function that has non-obvious behavior
- Add a `// Lost-wakeup fix:` comment every time you perform a post-enqueue CAS re-check

---

## Questions

Open a GitHub Discussion for questions that are not bugs or feature requests. Keep issues focused on actionable tasks.
