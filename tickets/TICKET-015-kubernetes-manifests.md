# TICKET-015: Kubernetes Deployment Manifests

**Type:** infra
**Priority:** P2
**Estimate:** M (3–4 days)
**Epic:** Enterprise Features
**Labels:** p2, sprint-11, kubernetes, infrastructure, deployment
**Status:** SHIPPED

**Tracking Note:** Implemented on `origin/main`. This ticket is retained as historical planning context; the design notes and checklists below may not match the final shipped implementation verbatim.

## Problem Statement

Organizations running Kubernetes need standard deployment artifacts to run this server in production. Without official manifests, each team invents its own deployment, leading to inconsistent configurations, missing health probes, no resource limits, and no autoscaling.

## Context

The server exposes:
- `/healthz` for liveness probes
- `/readyz` for readiness probes
- `/metrics` for Prometheus scraping
- Port 8085 (or configurable via `--addr`)

Configuration is via CLI flags or environment variables (after TICKET-004 env vars are implemented).

## Goals

1. Create `deploy/kubernetes/` directory with production-ready manifests.
2. Provide: `deployment.yaml`, `service.yaml`, `configmap.yaml`, `hpa.yaml`, `servicemonitor.yaml`, `rbac.yaml`.
3. Manifests should use resource limits, health probes, and non-root security context.
4. Document each manifest in this ticket and in inline YAML comments.

## Non-Goals

- Helm chart (TICKET-015b / future ticket).
- Multi-namespace installation.
- Ingress configuration (too environment-specific).
- Storage provisioning (the server is stateless by default; snapshot persistence is optional).

## Technical Design

### deployment.yaml

```yaml
apiVersion: apps/v1
kind: Deployment
metadata:
  name: syncprimitives
  namespace: default
  labels:
    app: syncprimitives
    version: "0.2.0"
spec:
  replicas: 2
  selector:
    matchLabels:
      app: syncprimitives
  strategy:
    type: RollingUpdate
    rollingUpdate:
      maxUnavailable: 1
      maxSurge: 1
  template:
    metadata:
      labels:
        app: syncprimitives
        version: "0.2.0"
      annotations:
        prometheus.io/scrape: "true"
        prometheus.io/port: "8085"
        prometheus.io/path: "/metrics"
    spec:
      securityContext:
        runAsNonRoot: true
        runAsUser: 65534   # nobody
        runAsGroup: 65534
        fsGroup: 65534
      containers:
      - name: syncprimitives
        image: ghcr.io/sanskarpan/advanced-synchronization-primitives:latest
        imagePullPolicy: Always
        ports:
        - name: http
          containerPort: 8085
          protocol: TCP
        envFrom:
        - configMapRef:
            name: syncprimitives-config
        env:
        - name: SYNCPRIM_API_KEY
          valueFrom:
            secretKeyRef:
              name: syncprimitives-secret
              key: api-key
              optional: true
        resources:
          requests:
            cpu: "100m"
            memory: "128Mi"
          limits:
            cpu: "500m"
            memory: "512Mi"
        livenessProbe:
          httpGet:
            path: /healthz
            port: http
          initialDelaySeconds: 10
          periodSeconds: 30
          failureThreshold: 3
          timeoutSeconds: 5
        readinessProbe:
          httpGet:
            path: /readyz
            port: http
          initialDelaySeconds: 5
          periodSeconds: 10
          failureThreshold: 3
          timeoutSeconds: 3
        securityContext:
          allowPrivilegeEscalation: false
          readOnlyRootFilesystem: true
          capabilities:
            drop: ["ALL"]
        volumeMounts:
        - name: tmp
          mountPath: /tmp
      volumes:
      - name: tmp
        emptyDir: {}
      terminationGracePeriodSeconds: 30
```

### service.yaml

```yaml
apiVersion: v1
kind: Service
metadata:
  name: syncprimitives
  namespace: default
  labels:
    app: syncprimitives
spec:
  type: ClusterIP
  selector:
    app: syncprimitives
  ports:
  - name: http
    port: 80
    targetPort: http
    protocol: TCP
```

### configmap.yaml

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: syncprimitives-config
  namespace: default
data:
  SYNCPRIM_ADDR: ":8085"
  SYNCPRIM_ORIGINS: ""       # empty = localhost only; set to "*" for all origins
  SYNCPRIM_MAX_CONNS: "1000"
  LOG_FORMAT: "json"
```

### hpa.yaml

```yaml
apiVersion: autoscaling/v2
kind: HorizontalPodAutoscaler
metadata:
  name: syncprimitives
  namespace: default
spec:
  scaleTargetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: syncprimitives
  minReplicas: 2
  maxReplicas: 10
  metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 70
  - type: Resource
    resource:
      name: memory
      target:
        type: Utilization
        averageUtilization: 80
  behavior:
    scaleDown:
      stabilizationWindowSeconds: 300   # 5 minutes before scaling down
      policies:
      - type: Pods
        value: 1
        periodSeconds: 60
```

### servicemonitor.yaml (Prometheus Operator)

```yaml
apiVersion: monitoring.coreos.com/v1
kind: ServiceMonitor
metadata:
  name: syncprimitives
  namespace: default
  labels:
    app: syncprimitives
spec:
  selector:
    matchLabels:
      app: syncprimitives
  endpoints:
  - port: http
    path: /metrics
    interval: 30s
    scrapeTimeout: 10s
```

### rbac.yaml

```yaml
# If using Kubernetes service account for future integrations
apiVersion: v1
kind: ServiceAccount
metadata:
  name: syncprimitives
  namespace: default
```

## Backend Implementation

No code changes required. The manifests reference the existing Docker image and endpoints.

## Frontend Implementation

None.

## Database / State Changes

For snapshot persistence in Kubernetes:
1. Mount a `PersistentVolumeClaim` at `/data/snapshot.json`.
2. Set `SYNCPRIM_SNAPSHOT_PATH=/data/snapshot.json` in the ConfigMap.
3. Note: with 2+ replicas, each pod has its own snapshot unless a shared PVC (ReadWriteMany) is used.

Add a `snapshot-pvc.yaml` as an optional manifest:
```yaml
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: syncprimitives-snapshot
spec:
  accessModes: [ReadWriteOnce]
  resources:
    requests:
      storage: 1Gi
```

## API Changes

None.

## Infrastructure Requirements

- Kubernetes 1.25+
- Prometheus Operator (optional, for ServiceMonitor)
- Container registry access for the image

## Edge Cases

- WebSocket connections behind a Kubernetes Ingress: many Ingress controllers (nginx, Traefik) require special annotations to proxy WebSocket connections. Document required nginx annotations:
  ```yaml
  nginx.ingress.kubernetes.io/proxy-read-timeout: "3600"
  nginx.ingress.kubernetes.io/proxy-send-timeout: "3600"
  nginx.ingress.kubernetes.io/proxy-http-version: "1.1"
  nginx.ingress.kubernetes.io/configuration-snippet: |
    proxy_set_header Upgrade $http_upgrade;
    proxy_set_header Connection "upgrade";
  ```
- `readOnlyRootFilesystem: true`: the server only writes to the configured snapshot path. The `tmp` emptyDir volume provides `/tmp` for any OS-level temporary files.
- Multiple replicas with in-memory state: each replica has independent state. Clients connecting to different replicas see different primitives. This is intentional for the current single-instance design.

## Failure Handling

- Pod crash: Kubernetes restarts the pod. With snapshot persistence, primitives are restored. Without it, state is lost on restart.
- OOM kill: Kubernetes kills the pod when memory limit is exceeded. The 512Mi limit should be sufficient for 1000 connections × moderate primitive count.

## Security Considerations

- `readOnlyRootFilesystem: true` prevents writing to the container's root filesystem.
- `allowPrivilegeEscalation: false` prevents the process from gaining elevated privileges.
- `capabilities: drop: ["ALL"]` removes all Linux capabilities.
- API key stored in a Kubernetes Secret (not a ConfigMap).
- Never store the API key in the container image or ConfigMap.

## Testing Plan

### Unit Tests

Manifest syntax validation:
```bash
kubectl apply --dry-run=client -f deploy/kubernetes/
```

### Integration Tests

Deploy to a local Kubernetes cluster (Kind or Minikube):
```bash
kind create cluster
kubectl apply -f deploy/kubernetes/
kubectl wait --for=condition=ready pod -l app=syncprimitives
curl http://localhost:8085/healthz  # via port-forward
```

### E2E Tests

Full deployment test: apply all manifests, connect a WebSocket client, create primitives, verify metrics endpoint, delete namespace, verify clean teardown.

## Monitoring Requirements

`ServiceMonitor` enables Prometheus scraping automatically. Verify with:
```bash
kubectl get servicemonitor syncprimitives
# Wait for Prometheus to pick up the target
kubectl port-forward svc/prometheus 9090:9090
# Navigate to http://localhost:9090/targets
```

## Logging Requirements

Set `LOG_FORMAT: "json"` in ConfigMap to ensure structured JSON logs that Kubernetes log aggregation (e.g., Fluentd, Loki) can parse.

## Metrics to Track

All `syncprim_*` metrics via Prometheus scraping at `/metrics`.

## Rollback Plan

```bash
kubectl delete -f deploy/kubernetes/
```

No data loss if no PVC was attached.

## Acceptance Criteria

- [ ] `kubectl apply --dry-run=client -f deploy/kubernetes/` succeeds without errors
- [ ] Deployment starts with 2 replicas
- [ ] Liveness probe (`/healthz`) configured with appropriate thresholds
- [ ] Readiness probe (`/readyz`) configured with appropriate thresholds
- [ ] HPA scales on CPU and memory
- [ ] API key sourced from Kubernetes Secret
- [ ] Resource requests and limits defined
- [ ] Non-root security context applied
- [ ] README deployment section references `deploy/kubernetes/`

## Definition of Done

- [ ] Code reviewed and merged
- [ ] Manifests validated against Kubernetes API with `kubectl apply --dry-run`
- [ ] Deployed and tested on Kind/Minikube
- [ ] README deployment section updated
- [ ] CHANGELOG entry written
