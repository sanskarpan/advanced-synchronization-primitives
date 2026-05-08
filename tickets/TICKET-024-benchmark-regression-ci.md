# TICKET-024: Benchmark Regression Tracking in CI

**Type:** infra
**Priority:** P2
**Estimate:** M (3 days)
**Epic:** Scalability and Performance
**Labels:** p2, sprint-13, ci, performance, benchmarks
**Status:** TODO

## Problem Statement

Performance regressions are easy to introduce accidentally. A change that appears safe (refactoring a lock implementation, adding a logging call) can degrade throughput by 20–50% on hot paths. Without automated benchmark comparison, these regressions go undetected until they cause production incidents or are discovered by a user.

The current CI pipeline runs benchmarks (`make bench`) but discards the results without comparing them to a baseline. There is no mechanism to fail a PR when performance degrades.

## Context

Current bench job in `.github/workflows/ci.yml`:
```yaml
bench:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: "1.21"
    - name: Benchmarks
      run: go test -bench=. -benchmem -timeout 120s ./internal/primitives/
```

Results are printed to CI logs but not stored or compared.

Benchmarks in `internal/primitives/`:
- `BenchmarkMutexUncontended` / `BenchmarkMutexContended`
- `BenchmarkRWLockRead` / `BenchmarkRWLockReadHeavy`
- `BenchmarkSemaphore` / `BenchmarkSemaphoreBurst`
- `BenchmarkBarrierSmall` / `BenchmarkBarrierLarge`

## Goals

1. Store benchmark results from the `main` branch as a baseline.
2. On each PR, compare the PR's benchmark results against the baseline.
3. Fail the CI job if any benchmark regresses by more than 20% in `ns/op`.
4. Post a comment on the PR with the benchmark comparison table.
5. Use `golang.org/x/perf/cmd/benchstat` for statistical comparison.

## Non-Goals

- Tracking benchmark trends over time (requires a time-series database; future work).
- Per-commit benchmark storage (only the latest main is the baseline).
- Memory (`B/op`, `allocs/op`) regression detection (only `ns/op` in scope).

## Technical Design

### Storing Baselines

Add a workflow job `bench-baseline` that runs on pushes to `main` and stores results as a GitHub Actions artifact:

```yaml
bench-baseline:
  if: github.ref == 'refs/heads/main'
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: "1.21"
    - name: Run benchmarks
      run: |
        go test -bench=. -benchmem -count=5 -timeout 300s ./internal/primitives/ > baseline.txt
    - name: Store baseline
      uses: actions/upload-artifact@v4
      with:
        name: bench-baseline
        path: baseline.txt
        retention-days: 90
```

### PR Comparison Job

Add a workflow job `bench-compare` that runs on PRs:

```yaml
bench-compare:
  if: github.event_name == 'pull_request'
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version: "1.21"

    - name: Download baseline
      uses: dawidd6/action-download-artifact@v2
      with:
        workflow: ci.yml
        branch: main
        name: bench-baseline
        path: baseline/

    - name: Run PR benchmarks
      run: |
        go test -bench=. -benchmem -count=5 -timeout 300s ./internal/primitives/ > pr.txt

    - name: Install benchstat
      run: go install golang.org/x/perf/cmd/benchstat@latest

    - name: Compare benchmarks
      run: |
        benchstat -delta-test none -confidence 0.95 baseline/baseline.txt pr.txt > comparison.txt
        cat comparison.txt

    - name: Check for regressions
      run: |
        # Fail if any benchmark regressed by more than 20%
        if grep -E '\+[2-9][0-9]\.[0-9]+%|\+[0-9]{3,}' comparison.txt; then
          echo "REGRESSION DETECTED: One or more benchmarks degraded >20%"
          exit 1
        fi

    - name: Comment PR with results
      if: always()
      uses: actions/github-script@v7
      with:
        script: |
          const fs = require('fs');
          const comparison = fs.readFileSync('comparison.txt', 'utf8');
          await github.rest.issues.createComment({
            issue_number: context.issue.number,
            owner: context.repo.owner,
            repo: context.repo.repo,
            body: `## Benchmark Comparison\n\`\`\`\n${comparison}\n\`\`\``
          });
```

### Baseline Unavailability

If no baseline artifact exists (e.g., first PR before `main` has run), skip the regression check:
```yaml
    - name: Download baseline
      id: baseline
      uses: dawidd6/action-download-artifact@v2
      continue-on-error: true
      with:
        workflow: ci.yml
        branch: main
        name: bench-baseline
        path: baseline/

    - name: Compare benchmarks
      if: steps.baseline.outcome == 'success'
      run: benchstat baseline/baseline.txt pr.txt
```

## Backend Implementation

No Go code changes required. This is a CI infrastructure change.

## Frontend Implementation

None.

## Database / State Changes

Benchmark results stored as GitHub Actions artifacts.

## API Changes

None.

## Infrastructure Requirements

- GitHub Actions runner with Go 1.21+.
- `actions/download-artifact@v2` action (or `dawidd6/action-download-artifact@v2` for cross-workflow artifact downloads).
- Sufficient runner time: 5 benchmark runs × all benchmarks ≈ 5 minutes.

## Edge Cases

- Baseline artifact expired (90 days): skip comparison, log warning.
- Benchmark not present in PR (renamed or deleted): `benchstat` reports `0%` change or omits it. Not treated as a regression.
- Flaky benchmarks: `-count=5` and the `-confidence 0.95` flag in `benchstat` provide statistical filtering to reduce false positives.
- PR runner hardware different from baseline runner: all jobs use `ubuntu-latest`. GitHub Actions runners are provisioned with comparable hardware, but there is inherent noise. The 20% threshold accommodates this.

## Failure Handling

- Regression detected: CI job fails. The PR comment shows which benchmarks regressed.
- Baseline not available: regression check is skipped with a notice in the CI log.
- `benchstat` parse error: job fails with an error. Check benchmark output format.

## Security Considerations

- The benchmark workflow does not access secrets (no API keys, no deployment credentials).
- Artifact downloads from the `main` branch baseline are scoped to the same repository.

## Testing Plan

### Unit Tests

None for a CI configuration change.

### Integration Tests

Manual: create a PR that deliberately introduces a ~30% slowdown in one benchmark (e.g., add an extra `time.Sleep(10*time.Nanosecond)` in a hot path). Verify the CI job fails with a regression message.

Manual: create a PR with no performance impact. Verify the CI job passes.

### E2E Tests

Submit the CI workflow to GitHub Actions on a test branch. Verify:
1. `bench-baseline` job runs on push to main and stores artifact.
2. `bench-compare` job runs on PR and downloads the artifact.
3. Comparison output is posted as a PR comment.

## Monitoring Requirements

None. CI job status is visible in GitHub.

## Logging Requirements

The `benchstat` comparison is the output. The `comparison.txt` file is uploaded as an artifact for inspection.

## Metrics to Track

No new application metrics. Track the number of benchmark regression failures over time (manually or via GitHub API).

## Rollback Plan

Remove the `bench-baseline` and `bench-compare` jobs from `ci.yml`. Performance regression detection is manual again.

## Acceptance Criteria

- [ ] `bench-baseline` job runs on pushes to `main` and stores `baseline.txt`
- [ ] `bench-compare` job runs on PRs and downloads the baseline
- [ ] `benchstat` comparison posted as PR comment
- [ ] CI fails when any benchmark regresses by >20% in ns/op
- [ ] CI skips regression check gracefully when no baseline is available
- [ ] `-count=5` used for statistical significance

## Definition of Done

- [ ] CI workflow updated and committed
- [ ] Manual test: regression PR fails, no-regression PR passes
- [ ] Documentation updated (CONTRIBUTING.md)
- [ ] CHANGELOG entry written
