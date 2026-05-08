# Deployment and Rollback Runbook

**Project:** Advanced Synchronization Primitives
**Version:** v0.2.0
**Updated:** 2026-05-09
**Owner:** Ops / Platform Team

This runbook covers all deployment scenarios: fresh installation, upgrade, configuration change, and rollback. It includes rollback procedures for each failure mode and defines the go/no-go criteria for each step.

---

## Prerequisites

Before starting any deployment:

- [ ] QA Checklist sign-off for the release (see `tickets/QA-CHECKLIST.md`)
- [ ] Security Checklist sign-off (see `tickets/SECURITY-CHECKLIST.md`)
- [ ] Performance Checklist sign-off (see `tickets/PERFORMANCE-CHECKLIST.md`)
- [ ] Docker image built and pushed to registry (`ghcr.io/sanskarpan/advanced-synchronization-primitives:<version>`)
- [ ] CI pipeline green on the release branch
- [ ] Monitoring dashboard visible and operational (Prometheus + Grafana)
- [ ] Rollback procedure reviewed and understood by the operator

---

## Environments

| Environment | URL | Purpose | Deployment Gate |
|-------------|-----|---------|----------------|
| dev | http://localhost:8085 | Developer testing | None |
| staging | https://syncprim-staging.internal | Integration testing | QA sign-off |
| production | https://syncprim.internal | Live traffic | Security + QA + Perf sign-off |

---

## 1. Fresh Installation

### 1.1 Docker (Single Instance)

```bash
# Pull the image
docker pull ghcr.io/sanskarpan/advanced-synchronization-primitives:v0.2.0

# Create a data directory for snapshots
mkdir -p /data/syncprim && chmod 750 /data/syncprim

# Start the server
docker run -d \
  --name syncprimitives \
  --restart unless-stopped \
  -p 8085:8085 \
  -v /data/syncprim:/data \
  -e LOG_FORMAT=json \
  -e SYNCPRIM_SNAPSHOT_PATH=/data/snapshot.json \
  -e SYNCPRIM_MAX_CONNS=1000 \
  -e SYNCPRIM_API_KEY="$(cat /run/secrets/syncprim-api-key)" \
  --memory=512m \
  --cpus=0.5 \
  ghcr.io/sanskarpan/advanced-synchronization-primitives:v0.2.0

# Verify health
curl -f http://localhost:8085/healthz
curl -f http://localhost:8085/readyz
```

**Expected output for `/healthz`:**
```json
{"status": "ok", "uptime": "...", "dropped_broadcasts": 0}
```

**Go/No-Go:** Proceed only if both `/healthz` and `/readyz` return HTTP 200 within 30 seconds.

### 1.2 Kubernetes

```bash
# Apply all manifests
kubectl apply -f deploy/kubernetes/

# Wait for deployment to become ready
kubectl rollout status deployment/syncprimitives --timeout=120s

# Verify pods are running
kubectl get pods -l app=syncprimitives

# Check logs
kubectl logs -l app=syncprimitives --tail=50

# Test health endpoints via port-forward
kubectl port-forward svc/syncprimitives 8085:80 &
curl -f http://localhost:8085/healthz
curl -f http://localhost:8085/readyz
```

**Go/No-Go:** All pods in `Running` state, readiness probe passing, health check returns 200.

---

## 2. Upgrade Procedure

### Pre-Upgrade Checklist

- [ ] Take note of current primitive count: `curl /metrics | grep syncprim_primitives_total`
- [ ] Note current connection count: `curl /healthz`
- [ ] Confirm snapshot is being persisted: check that `/data/snapshot.json` exists and was recently modified
- [ ] Notify users of maintenance window (if planned downtime expected)

### 2.1 Docker Upgrade (Zero-Downtime Rolling)

```bash
# Pull new image
docker pull ghcr.io/sanskarpan/advanced-synchronization-primitives:v0.2.0

# Graceful shutdown of existing container (sends SIGTERM)
docker stop --time=15 syncprimitives

# Wait for stop to complete
docker wait syncprimitives

# Verify snapshot was written
ls -la /data/syncprim/snapshot.json
# File should have been modified within the last 60 seconds

# Start new container with same configuration
docker run -d \
  --name syncprimitives \
  [... same flags as fresh install ...]
  ghcr.io/sanskarpan/advanced-synchronization-primitives:v0.2.0

# Verify health
sleep 5
curl -f http://localhost:8085/healthz
```

**Expected:** Server restores primitives from snapshot. `/healthz` responds within 5 seconds.

### 2.2 Kubernetes Rolling Upgrade

```bash
# Update the image tag in the deployment
kubectl set image deployment/syncprimitives \
  syncprimitives=ghcr.io/sanskarpan/advanced-synchronization-primitives:v0.2.0

# Monitor the rollout
kubectl rollout status deployment/syncprimitives --timeout=120s

# Verify all pods are running the new image
kubectl get pods -l app=syncprimitives -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.containers[0].image}{"\n"}{end}'

# Check logs of new pods for startup errors
kubectl logs -l app=syncprimitives --tail=100
```

**Kubernetes rolling update behavior:**
- 1 pod updated at a time (`maxUnavailable: 1`)
- New pod must pass readiness probe before old pod is terminated
- If new pod fails readiness, rollout pauses automatically

**Go/No-Go:** `kubectl rollout status` exits 0. All pods running. `/healthz` returns 200 from each pod.

---

## 3. Configuration Change

### Changing API Key

```bash
# Docker
docker stop --time=15 syncprimitives
docker rm syncprimitives
docker run -d --name syncprimitives \
  -e SYNCPRIM_API_KEY="new-api-key" \
  [... other flags ...] \
  ghcr.io/sanskarpan/advanced-synchronization-primitives:v0.2.0
```

```bash
# Kubernetes — update the secret
kubectl create secret generic syncprimitives-secret \
  --from-literal=api-key="new-api-key" \
  --dry-run=client -o yaml | kubectl apply -f -

# Restart pods to pick up the new secret
kubectl rollout restart deployment/syncprimitives
kubectl rollout status deployment/syncprimitives
```

### Changing Allowed Origins

```bash
# Update ConfigMap
kubectl edit configmap syncprimitives-config
# Set SYNCPRIM_ORIGINS to the new value

# Restart pods
kubectl rollout restart deployment/syncprimitives
```

### Changing Connection Limit

```bash
# Update ConfigMap
kubectl patch configmap syncprimitives-config \
  --type merge \
  --patch '{"data":{"SYNCPRIM_MAX_CONNS":"2000"}}'

# Restart pods
kubectl rollout restart deployment/syncprimitives
```

---

## 4. Rollback Procedures

### 4.1 Rollback Triggers (Automatic)

The following conditions should trigger an immediate rollback:
- `syncprim_dropped_broadcasts_total` rate > 100/minute (server is unable to keep up with broadcasts)
- `/readyz` returns HTTP 503 for > 60 seconds
- Error rate in `/metrics` > 5% of operations
- Pod OOMKilled (memory limit exceeded)
- Panics in server logs (`level=ERROR msg="panic recovered"`)

### 4.2 Docker Rollback

```bash
# Stop the current container
docker stop --time=15 syncprimitives
docker rm syncprimitives

# Start the previous version
docker run -d \
  --name syncprimitives \
  [... same configuration flags ...] \
  ghcr.io/sanskarpan/advanced-synchronization-primitives:v0.1.0

# Verify health
sleep 5
curl -f http://localhost:8085/healthz
```

**Note on snapshot compatibility:** If the new version changed the snapshot format and wrote a new-format snapshot before the rollback, the old version may not be able to read it. In this case, rename or remove the snapshot file before starting the old version. Primitives will be lost but the server will start cleanly.

```bash
# If snapshot format changed and old version cannot read it:
mv /data/syncprim/snapshot.json /data/syncprim/snapshot.json.bak
```

### 4.3 Kubernetes Rollback

```bash
# Immediate rollback to previous deployment
kubectl rollout undo deployment/syncprimitives

# Monitor rollback
kubectl rollout status deployment/syncprimitives --timeout=120s

# Verify
kubectl get pods -l app=syncprimitives
curl -f http://localhost:8085/healthz
```

For a rollback to a specific revision:
```bash
# List revision history
kubectl rollout history deployment/syncprimitives

# Roll back to revision N
kubectl rollout undo deployment/syncprimitives --to-revision=N
```

---

## 5. Smoke Tests After Deployment

Run these tests immediately after any deployment or configuration change:

```bash
BASE_URL="http://localhost:8085"
API_KEY="your-api-key"

# 1. Health check
curl -f "$BASE_URL/healthz" | jq .

# 2. Readiness check
curl -f "$BASE_URL/readyz"

# 3. Metrics
curl -s "$BASE_URL/metrics" | grep "syncprim_" | head -10

# 4. WebSocket connection (requires websocat or wscat)
echo '{"type":"createMutex","payload":{"id":"smoke-test-1","name":"smoke"}}' | \
  websocat -H "Authorization: Bearer $API_KEY" \
    "ws://localhost:8085/ws" --one-message

# 5. Dashboard accessible
curl -s "$BASE_URL/" | grep -c "syncprim" || echo "Dashboard HTML not found"
```

**Expected:** All commands exit 0 with expected output. No error messages in logs for 5 minutes after deployment.

---

## 6. Post-Deployment Monitoring

Monitor these metrics for 30 minutes after a deployment:

| Metric | Acceptable Range | Alert Threshold |
|--------|-----------------|-----------------|
| `/healthz` response time | < 50 ms | > 200 ms |
| `syncprim_dropped_broadcasts_total` rate | < 1/minute | > 10/minute |
| Pod memory usage | < 400 MiB | > 480 MiB (close to 512 MiB limit) |
| Pod CPU usage | < 0.4 cores | > 0.45 cores |
| Active WebSocket connections | per-capacity | > 90% of `MaxConns` |
| Error responses per minute | < 1% of operations | > 5% |

Dashboard link: (fill in your Grafana/Prometheus dashboard URL)

---

## 7. Incident Response

### Server Not Responding

1. Check pod status: `kubectl get pods -l app=syncprimitives`
2. Check pod logs: `kubectl logs <pod-name> --tail=100`
3. Check events: `kubectl describe pod <pod-name>`
4. If pod is in `CrashLoopBackOff`: collect logs, then rollback.
5. If pod is in `OOMKilled`: increase memory limit in Deployment spec, redeploy.

### WebSocket Connections Failing

1. Check origin allow list (`SYNCPRIM_ORIGINS`).
2. Check API key (`SYNCPRIM_API_KEY`).
3. Check if connection cap is reached (`syncprim_active_connections` metric).
4. Check nginx/ingress proxy configuration for WebSocket headers.

### High Memory Usage

1. Capture heap profile: `kubectl exec <pod> -- curl http://localhost:8085/debug/pprof/heap > heap.out`
2. Analyze: `go tool pprof -top heap.out`
3. If due to goroutine accumulation: likely a goroutine leak (TICKET-010 mitigates this).
4. If due to large snapshots: reduce `MaxConns` to limit primitive count.
5. Rollback if memory continues to grow.

### Snapshot Corruption

Signs: server logs show `"failed to parse snapshot file"` on startup.

```bash
# Option 1: Move the corrupt snapshot and start with empty state
kubectl exec <pod> -- mv /data/snapshot.json /data/snapshot.json.corrupt

# Restart pod
kubectl delete pod <pod-name>

# Verify server starts with empty state
kubectl logs <new-pod-name> | grep -i snapshot
```

---

## 8. Maintenance Window Procedures

### Draining Connections for Planned Maintenance

```bash
# Send SIGTERM to start graceful shutdown
# Kubernetes does this automatically during pod deletion

# For Docker:
docker kill --signal=SIGTERM syncprimitives
# Server will:
# 1. Stop accepting new connections (503 on /readyz after TICKET-009)
# 2. Send close code 1001 to all active connections
# 3. Wait up to 5s for connections to close
# 4. Save snapshot
# 5. Exit
```

### Database/State Backup

```bash
# Backup the current snapshot file
cp /data/syncprim/snapshot.json /backups/snapshot-$(date +%Y%m%d-%H%M%S).json
```

---

## 9. Environment-Specific Notes

### Development

- No TLS required.
- API key optional.
- `--allowed-origins *` acceptable.
- Log format: `text` (default).

### Staging

- TLS with self-signed cert acceptable.
- API key required.
- Allowed origins: staging domain only.
- Log format: `json`.
- Snapshot persistence: `/data/snapshot.json`.

### Production

- TLS with CA-signed certificate required.
- API key required (or JWT secret for TICKET-016+).
- Allowed origins: production domain only (never `*`).
- Log format: `json`.
- Snapshot persistence: on a persistent volume.
- `MaxConns`: 1000 (or as capacity-planned).
- Resource limits: CPU 500m, Memory 512Mi.

---

## 10. Emergency Contact

In case of a production incident that cannot be resolved with this runbook:

1. Escalate to the platform on-call via PagerDuty.
2. File a P0 incident ticket with symptoms, steps taken, and current state.
3. If the server is completely down, consider the impact: all in-flight primitive operations are lost. Clients will reconnect but primitive state (unless persisted via snapshot) will be empty.
