package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sanskar/syncprimitives/internal/auth"
)

func TestListJSON(t *testing.T) {
	ts := newMockWSServer(t, "")
	code, out, stderr := runForTest([]string{"--server", ts.wsURL(), "--json", "list"}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr)
	}

	var rows []map[string]interface{}
	if err := json.Unmarshal([]byte(out), &rows); err != nil {
		t.Fatalf("invalid json output: %v\n%s", err, out)
	}
	if len(rows) != 0 {
		t.Fatalf("expected no primitives, got %d", len(rows))
	}
}

func TestCreateMutexJSON(t *testing.T) {
	ts := newMockWSServer(t, "")
	code, out, stderr := runForTest([]string{"--server", ts.wsURL(), "--json", "create", "mutex", "mu-cli"}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, "\"result\": \"ok\"") {
		t.Fatalf("expected ok json output, got: %s", out)
	}
}

func TestListAuthViaEnv(t *testing.T) {
	ts := newMockWSServer(t, "secret123")
	env := map[string]string{"SYNCPRIM_API_KEY": "secret123"}

	code, out, stderr := runForTest([]string{"--server", ts.wsURL(), "--json", "list"}, env)
	if code != 0 {
		t.Fatalf("expected exit 0 with env api key, got %d stderr=%s", code, stderr)
	}
	if !strings.Contains(out, "[") {
		t.Fatalf("expected json list output, got: %s", out)
	}
}

func TestConnectFailure(t *testing.T) {
	code, _, stderr := runForTest([]string{"--server", "ws://127.0.0.1:65534/ws", "list"}, nil)
	if code != 1 {
		t.Fatalf("expected exit 1 on connect failure, got %d", code)
	}
	if !strings.Contains(stderr, "failed to connect") {
		t.Fatalf("expected connect failure message, got: %s", stderr)
	}
}

func TestTokenGenerate(t *testing.T) {
	code, out, stderr := runForTest([]string{"token", "generate", "--secret", "jwt-secret", "--sub", "alice", "--role", "viewer", "--namespace", "team-a", "--ttl", "2m"}, nil)
	if code != 0 {
		t.Fatalf("expected exit 0, got %d stderr=%s", code, stderr)
	}

	token := strings.TrimSpace(out)
	claims, err := auth.ValidateJWT(token, "jwt-secret")
	if err != nil {
		t.Fatalf("expected valid generated token, got %v", err)
	}
	if claims.Sub != "alice" {
		t.Fatalf("expected sub alice, got %q", claims.Sub)
	}
	if claims.Role != "viewer" {
		t.Fatalf("expected role viewer, got %q", claims.Role)
	}
	if claims.Namespace != "team-a" {
		t.Fatalf("expected namespace team-a, got %q", claims.Namespace)
	}
}

func TestTokenGenerateRequiresSecret(t *testing.T) {
	code, _, stderr := runForTest([]string{"token", "generate", "--sub", "alice"}, nil)
	if code != 2 {
		t.Fatalf("expected exit 2, got %d", code)
	}
	if !strings.Contains(stderr, "--secret is required") {
		t.Fatalf("expected secret validation error, got: %s", stderr)
	}
}

func runForTest(args []string, env map[string]string) (int, string, string) {
	if env == nil {
		env = map[string]string{}
	}
	getenv := func(k string) string { return env[k] }
	var out strings.Builder
	var err strings.Builder
	code := run(args, getenv, &out, &err)
	return code, out.String(), err.String()
}

type mockWSServer struct {
	t  *testing.T
	ts *httptest.Server

	mu         sync.Mutex
	primitives map[string]primitiveInfo
	authKey    string
}

func newMockWSServer(t *testing.T, authKey string) *mockWSServer {
	t.Helper()
	m := &mockWSServer{t: t, primitives: map[string]primitiveInfo{}, authKey: authKey}
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", m.handleWS)
	m.ts = httptest.NewServer(mux)
	t.Cleanup(m.ts.Close)
	return m
}

func (m *mockWSServer) wsURL() string {
	return "ws" + strings.TrimPrefix(m.ts.URL, "http") + "/ws"
}

func (m *mockWSServer) handleWS(w http.ResponseWriter, r *http.Request) {
	if m.authKey != "" {
		auth := r.Header.Get("Authorization")
		expected := "Bearer " + m.authKey
		if auth != expected {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}

	upgrader := websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	_ = conn.WriteJSON(wsMessage{Type: "initialState", Payload: m.initialStateJSON()})

	for {
		_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		var msg wsMessage
		if err := conn.ReadJSON(&msg); err != nil {
			return
		}
		switch msg.Type {
		case "createMutex":
			var p struct {
				ID   string `json:"id"`
				Name string `json:"name"`
			}
			if json.Unmarshal(msg.Payload, &p) != nil || p.ID == "" {
				_ = conn.WriteJSON(wsMessage{Type: "error", Payload: rawJSON(`{"message":"Invalid payload"}`)})
				continue
			}
			m.mu.Lock()
			m.primitives[p.ID] = primitiveInfo{
				ID:        p.ID,
				Type:      "Mutex",
				Name:      p.Name,
				CreatedAt: time.Now(),
				Stats: map[string]interface{}{
					"IsLocked": false,
				},
			}
			m.mu.Unlock()
			_ = conn.WriteJSON(wsMessage{Type: "success", Payload: rawJSON(`{"message":"Mutex created"}`)})
		default:
			_ = conn.WriteJSON(wsMessage{Type: "error", Payload: rawJSON(`{"message":"unsupported in mock"}`)})
		}
	}
}

func (m *mockWSServer) initialStateJSON() json.RawMessage {
	m.mu.Lock()
	defer m.mu.Unlock()
	state := map[string]interface{}{
		"primitives": m.primitives,
		"goroutines": map[string]interface{}{},
		"events":     []interface{}{},
		"metrics":    map[string]interface{}{},
	}
	b, err := json.Marshal(state)
	if err != nil {
		m.t.Fatalf("marshal mock state: %v", err)
	}
	return b
}

func rawJSON(v string) json.RawMessage {
	return json.RawMessage([]byte(v))
}
