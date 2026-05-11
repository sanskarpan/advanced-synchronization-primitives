package kubernetes_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readManifest(t *testing.T, name string) string {
	t.Helper()
	path := filepath.Join(".", name)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(data)
}

func requireAll(t *testing.T, body string, snippets ...string) {
	t.Helper()
	for _, snippet := range snippets {
		if !strings.Contains(body, snippet) {
			t.Fatalf("expected manifest to contain %q", snippet)
		}
	}
}

func TestDeploymentManifestCoversRuntimeConfiguration(t *testing.T) {
	body := readManifest(t, "deployment.yaml")
	requireAll(t, body,
		"kind: Deployment",
		"replicas: 2",
		"/healthz",
		"/readyz",
		"allowPrivilegeEscalation: false",
		"readOnlyRootFilesystem: true",
		`drop: ["ALL"]`,
		`-default-namespace "${SYNCPRIM_DEFAULT_NAMESPACE:-default}"`,
		`-max-conns "${SYNCPRIM_MAX_CONNS:-1000}"`,
		`-jwt-secret "${SYNCPRIM_JWT_SECRET}"`,
		`-snapshot-path "${SYNCPRIM_SNAPSHOT_PATH}"`,
		`-audit-log-path "${SYNCPRIM_AUDIT_LOG_PATH}"`,
		"name: syncprimitives-secret",
	)
}

func TestSupportingKubernetesManifestsExist(t *testing.T) {
	tests := []struct {
		name     string
		snippets []string
	}{
		{
			name: "configmap.yaml",
			snippets: []string{
				"kind: ConfigMap",
				"SYNCPRIM_ALLOWED_ORIGINS",
				"SYNCPRIM_DEFAULT_NAMESPACE",
				"SYNCPRIM_MAX_CONNS",
			},
		},
		{
			name: "service.yaml",
			snippets: []string{
				"kind: Service",
				"type: ClusterIP",
				"targetPort: http",
			},
		},
		{
			name: "hpa.yaml",
			snippets: []string{
				"kind: HorizontalPodAutoscaler",
				"averageUtilization: 70",
				"averageUtilization: 80",
			},
		},
		{
			name: "rbac.yaml",
			snippets: []string{
				"kind: ServiceAccount",
				"name: syncprimitives",
			},
		},
		{
			name: "servicemonitor.yaml",
			snippets: []string{
				"kind: ServiceMonitor",
				"path: /metrics",
				"interval: 30s",
			},
		},
		{
			name: "snapshot-pvc.yaml",
			snippets: []string{
				"kind: PersistentVolumeClaim",
				"syncprimitives-snapshot",
				"storage: 1Gi",
			},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			requireAll(t, readManifest(t, tc.name), tc.snippets...)
		})
	}
}

func TestReadmeReferencesKubernetesDeploymentAssets(t *testing.T) {
	data, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatalf("read README.md: %v", err)
	}
	body := string(data)
	requireAll(t, body,
		"kubectl apply -f deploy/kubernetes/",
		"syncprimitives-secret",
		"snapshot-pvc.yaml",
		"ServiceMonitor",
	)
}
