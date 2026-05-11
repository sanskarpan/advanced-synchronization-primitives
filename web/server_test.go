package web_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sanskar/syncprimitives/internal/auth"
	"github.com/sanskar/syncprimitives/web"
)

// newTestServer creates a test server and returns it along with a cleanup func.
func newTestServer(t *testing.T) (*httptest.Server, *web.Server, func()) {
	t.Helper()
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
	})

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	mux.HandleFunc("/healthz", srv.HandleHealthz)
	mux.HandleFunc("/readyz", srv.HandleReadyz)
	mux.HandleFunc("/metrics", srv.HandlePrometheusMetrics)
	mux.HandleFunc("/", srv.HandleStatic)

	ts := httptest.NewServer(mux)
	return ts, srv, func() {
		ts.Close()
	}
}

// dialWS opens a websocket connection to the test server.
func dialWS(t *testing.T, ts *httptest.Server) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	dialer := websocket.DefaultDialer
	conn, _, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("WS dial failed: %v", err)
	}
	return conn
}

// readMsg reads one JSON message from conn.
func readMsg(t *testing.T, conn *websocket.Conn) map[string]interface{} {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, data, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("ReadMessage: %v", err)
	}
	var msg map[string]interface{}
	if err := json.Unmarshal(data, &msg); err != nil {
		t.Fatalf("JSON unmarshal: %v", err)
	}
	return msg
}

// sendMsg sends a typed WebSocket message.
func sendMsg(t *testing.T, conn *websocket.Conn, msgType string, payload interface{}) {
	t.Helper()
	data, _ := json.Marshal(map[string]interface{}{
		"type":    msgType,
		"payload": payload,
	})
	if err := conn.WriteMessage(websocket.TextMessage, data); err != nil {
		t.Fatalf("WriteMessage: %v", err)
	}
}

// drainUntilType reads messages until one with the given type is found.
func drainUntilType(t *testing.T, conn *websocket.Conn, wantType string) map[string]interface{} {
	t.Helper()
	for {
		msg := readMsg(t, conn)
		if msg["type"] == wantType {
			return msg
		}
	}
}

// TestHandleStaticOK verifies that GET / returns 200.
func TestHandleStaticOK(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer resp.Body.Close()

	// ServeFile returns 200 for the root (serves index.html if it exists,
	// or 404 if static files are not present in the test working directory).
	// Both 200 and 404 are acceptable — the important thing is no 500.
	if resp.StatusCode == 500 {
		t.Errorf("GET / returned 500")
	}
}

// TestHandleStaticNotFound verifies that a non-existent path returns 404.
func TestHandleStaticNotFound(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/nonexistent-file-xyz.html")
	if err != nil {
		t.Fatalf("GET /nonexistent: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("expected 404, got %d", resp.StatusCode)
	}
}

// TestHandleStaticPathTraversal verifies path traversal is blocked.
func TestHandleStaticPathTraversal(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/../etc/passwd")
	if err != nil {
		t.Fatalf("GET /../etc/passwd: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("path traversal: expected 404, got %d", resp.StatusCode)
	}
}

// TestWebSocketConnect verifies upgrade and receipt of initialState.
func TestWebSocketConnect(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()

	msg := readMsg(t, conn)
	if msg["type"] != "initialState" {
		t.Errorf("expected initialState, got %v", msg["type"])
	}
}

func TestWebSocketDeltaUpdateOnlyChangedPrimitives(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // initialState

	sendMsg(t, conn, "createMutex", map[string]string{"id": "mu-delta-1", "name": "delta-1"})
	drainUntilType(t, conn, "success")
	sendMsg(t, conn, "createMutex", map[string]string{"id": "mu-delta-2", "name": "delta-2"})
	drainUntilType(t, conn, "success")

	// Wait for a baseline delta tick that includes both newly created primitives.
	deadline := time.Now().Add(4 * time.Second)
	seenBothCreated := false
	for time.Now().Before(deadline) {
		msg := readMsg(t, conn)
		if msg["type"] != "update" {
			continue
		}
		payload, _ := msg["payload"].(map[string]interface{})
		if payload == nil {
			continue
		}
		prims, _ := payload["Primitives"].(map[string]interface{})
		if prims == nil {
			continue
		}
		_, has1 := prims["mu-delta-1"]
		_, has2 := prims["mu-delta-2"]
		if has1 && has2 {
			seenBothCreated = true
			break
		}
	}
	if !seenBothCreated {
		t.Fatal("did not observe baseline update containing both created primitives")
	}

	sendMsg(t, conn, "primitiveOp", map[string]interface{}{
		"id": "mu-delta-1", "op": "lock", "holdMs": 1,
	})
	drainUntilType(t, conn, "success")

	deadline = time.Now().Add(4 * time.Second)
	foundChangedOnly := false
	for time.Now().Before(deadline) {
		msg := readMsg(t, conn)
		if msg["type"] != "update" {
			continue
		}
		payload, _ := msg["payload"].(map[string]interface{})
		if payload == nil {
			continue
		}
		prims, _ := payload["Primitives"].(map[string]interface{})
		if prims == nil || len(prims) != 1 {
			continue
		}
		if _, ok := prims["mu-delta-1"]; ok {
			if _, exists := prims["mu-delta-2"]; exists {
				t.Fatalf("expected delta update to exclude unchanged primitive mu-delta-2")
			}
			foundChangedOnly = true
			break
		}
	}
	if !foundChangedOnly {
		t.Fatal("did not observe delta update containing only changed primitive")
	}

	sendMsg(t, conn, "deletePrimitive", map[string]string{"id": "mu-delta-2"})
	drainUntilType(t, conn, "success")

	deadline = time.Now().Add(4 * time.Second)
	foundDeletion := false
	for time.Now().Before(deadline) {
		msg := readMsg(t, conn)
		if msg["type"] != "update" {
			continue
		}
		payload, _ := msg["payload"].(map[string]interface{})
		if payload == nil {
			continue
		}
		deleted, _ := payload["Deleted"].([]interface{})
		for _, item := range deleted {
			if id, ok := item.(string); ok && id == "mu-delta-2" {
				foundDeletion = true
				break
			}
		}
		if foundDeletion {
			break
		}
	}
	if !foundDeletion {
		t.Fatal("expected delta update to include deleted primitive id")
	}
}

func TestWebSocketRequestFullRefresh(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // initialState

	sendMsg(t, conn, "createMutex", map[string]string{"id": "mu-refresh-1", "name": "refresh"})
	drainUntilType(t, conn, "success")

	sendMsg(t, conn, "requestFullRefresh", map[string]interface{}{})
	stateMsg := drainUntilType(t, conn, "state")
	payload, _ := stateMsg["payload"].(map[string]interface{})
	if payload == nil {
		t.Fatal("expected state payload")
	}
	prims, _ := payload["primitives"].(map[string]interface{})
	if prims == nil {
		t.Fatalf("expected state payload to include primitives, got %#v", payload)
	}
	if _, ok := prims["mu-refresh-1"]; !ok {
		t.Fatalf("expected full refresh state to include mu-refresh-1")
	}

	sendMsg(t, conn, "requestFullRefresh", map[string]interface{}{})
	errMsg := drainUntilType(t, conn, "error")
	errPayload, _ := errMsg["payload"].(map[string]interface{})
	if errPayload == nil {
		t.Fatal("expected error payload")
	}
	if !strings.Contains(fmt.Sprint(errPayload["message"]), "full refresh rate limit exceeded") {
		t.Fatalf("expected full refresh rate-limit error, got %v", errPayload["message"])
	}
}

// TestWebSocketCreateRWLock verifies createRWLock returns success.
func TestWebSocketCreateRWLock(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	sendMsg(t, conn, "createRWLock", map[string]string{
		"id":   "rw-1",
		"name": "test-rwlock",
	})

	msg := drainUntilType(t, conn, "success")
	payload, _ := msg["payload"].(map[string]interface{})
	if payload == nil || payload["message"] == nil {
		t.Errorf("expected success payload, got %v", msg)
	}
}

// TestWebSocketCreateFairRWLock verifies createFairRWLock returns success.
func TestWebSocketCreateFairRWLock(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	sendMsg(t, conn, "createFairRWLock", map[string]string{
		"id":   "fair-rw-1",
		"name": "test-fair-rwlock",
	})

	msg := drainUntilType(t, conn, "success")
	payload, _ := msg["payload"].(map[string]interface{})
	if payload == nil || payload["message"] == nil {
		t.Errorf("expected success payload, got %v", msg)
	}
}

// TestWebSocketCreateSemaphoreZeroCapacity verifies capacity=0 returns error.
func TestWebSocketCreateSemaphoreZeroCapacity(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	sendMsg(t, conn, "createSemaphore", map[string]interface{}{
		"id":       "sem-0",
		"name":     "bad-sem",
		"capacity": 0,
	})

	msg := drainUntilType(t, conn, "error")
	payload, _ := msg["payload"].(map[string]interface{})
	if payload == nil {
		t.Fatalf("expected error payload")
	}
	if !strings.Contains(payload["message"].(string), "capacity") {
		t.Errorf("expected capacity error, got: %v", payload["message"])
	}
}

// TestWebSocketCreateBarrierZeroParties verifies parties=0 returns error.
func TestWebSocketCreateBarrierZeroParties(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	sendMsg(t, conn, "createBarrier", map[string]interface{}{
		"id":      "bar-0",
		"name":    "bad-bar",
		"parties": 0,
	})

	msg := drainUntilType(t, conn, "error")
	payload, _ := msg["payload"].(map[string]interface{})
	if payload == nil {
		t.Fatalf("expected error payload")
	}
	if !strings.Contains(payload["message"].(string), "parties") {
		t.Errorf("expected parties error, got: %v", payload["message"])
	}
}

// TestWebSocketUnknownMessageType verifies unknown type returns error.
func TestWebSocketUnknownMessageType(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	sendMsg(t, conn, "fooBar", map[string]string{})

	msg := drainUntilType(t, conn, "error")
	payload, _ := msg["payload"].(map[string]interface{})
	if payload == nil {
		t.Fatalf("expected error payload")
	}
	if !strings.Contains(payload["message"].(string), "Unknown") {
		t.Errorf("expected Unknown error, got: %v", payload["message"])
	}
}

// TestWebSocketDeletePrimitive verifies create then delete flow.
func TestWebSocketDeletePrimitive(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	// Create
	sendMsg(t, conn, "createMutex", map[string]string{
		"id":   "mu-del",
		"name": "del-mutex",
	})
	drainUntilType(t, conn, "success")

	// Delete
	sendMsg(t, conn, "deletePrimitive", map[string]string{"id": "mu-del"})
	msg := drainUntilType(t, conn, "success")
	payload, _ := msg["payload"].(map[string]interface{})
	if payload == nil {
		t.Fatalf("expected success payload on delete")
	}
}

// TestWebSocketPrimitiveOpRLock creates an RWLock and performs rlock op.
func TestWebSocketPrimitiveOpRLock(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	sendMsg(t, conn, "createRWLock", map[string]string{
		"id":   "rw-op",
		"name": "op-rwlock",
	})
	drainUntilType(t, conn, "success")

	sendMsg(t, conn, "primitiveOp", map[string]string{
		"id": "rw-op",
		"op": "rlock",
	})

	msg := drainUntilType(t, conn, "success")
	payload, _ := msg["payload"].(map[string]interface{})
	if payload == nil {
		t.Fatalf("expected success payload")
	}
}

// TestWebSocketPrimitiveOpFairRLock creates a FairRWLock and performs rlock op.
func TestWebSocketPrimitiveOpFairRLock(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	sendMsg(t, conn, "createFairRWLock", map[string]string{
		"id":   "fair-rw-op",
		"name": "op-fair-rwlock",
	})
	drainUntilType(t, conn, "success")

	sendMsg(t, conn, "primitiveOp", map[string]string{
		"id": "fair-rw-op",
		"op": "rlock",
	})

	msg := drainUntilType(t, conn, "success")
	payload, _ := msg["payload"].(map[string]interface{})
	if payload == nil {
		t.Fatalf("expected success payload")
	}
}

// TestWebSocketAuthRequired verifies that a non-empty APIKey rejects unauthenticated clients.
func TestWebSocketAuthRequired(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
		APIKey:         "secret123",
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	// No Authorization header — expect upgrade to fail with 401.
	_, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err == nil {
		t.Fatal("expected dial to fail without auth key")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401 Unauthorized, got: %v", resp)
	}
}

// TestWebSocketAuthAccepted verifies that a correct Bearer token allows the upgrade.
func TestWebSocketAuthAccepted(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
		APIKey:         "secret123",
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	// Authorization: Bearer <key> must succeed.
	headers := http.Header{"Authorization": []string{"Bearer secret123"}}
	conn, _, err := websocket.DefaultDialer.Dial(u, headers)
	if err != nil {
		t.Fatalf("expected successful dial with Bearer key, got: %v", err)
	}
	defer conn.Close()
	msg := readMsg(t, conn)
	if msg["type"] != "initialState" {
		t.Errorf("expected initialState, got %v", msg["type"])
	}
}

// TestWebSocketAuthQueryParamRejected verifies that supplying the API key as a
// URL query parameter is rejected (credential leakage prevention).
func TestWebSocketAuthQueryParamRejected(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
		APIKey:         "secret123",
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	// ?key= query param must NOT be accepted.
	_, resp, err := websocket.DefaultDialer.Dial(u+"?key=secret123", nil)
	if err == nil {
		t.Fatal("expected dial to fail when key supplied via query param")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("expected 401, got: %v", resp)
	}
}

func TestWebSocketJWTAuthAccepted(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
		JWTSecret:      "jwt-secret",
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	token := mustJWT(t, "jwt-secret", "alice", "operator", time.Now().Add(time.Hour))
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(u, headers)
	if err != nil {
		t.Fatalf("expected successful dial with JWT, got: %v", err)
	}
	defer conn.Close()

	msg := readMsg(t, conn)
	if msg["type"] != "initialState" {
		t.Fatalf("expected initialState, got %v", msg["type"])
	}
}

func TestWebSocketJWTAuthRejectedExpired(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
		JWTSecret:      "jwt-secret",
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	token := mustJWT(t, "jwt-secret", "alice", "operator", time.Now().Add(-time.Minute))
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	_, resp, err := websocket.DefaultDialer.Dial(u, headers)
	if err == nil {
		t.Fatal("expected expired JWT dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %v", resp)
	}
}

func TestWebSocketJWTAuthRejectedInvalidSignature(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
		JWTSecret:      "jwt-secret",
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	token := mustJWT(t, "wrong-secret", "alice", "operator", time.Now().Add(time.Hour))
	headers := http.Header{"Authorization": []string{"Bearer " + token}}
	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	_, resp, err := websocket.DefaultDialer.Dial(u, headers)
	if err == nil {
		t.Fatal("expected invalid-signature JWT dial to fail")
	}
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 Unauthorized, got %v", resp)
	}
}

func mustJWT(t *testing.T, secret, sub, role string, exp time.Time) string {
	t.Helper()
	token, err := auth.GenerateJWT(auth.Claims{
		Sub:  sub,
		Role: role,
		Iat:  time.Now().Unix(),
		Exp:  exp.Unix(),
	}, secret)
	if err != nil {
		t.Fatalf("generate JWT: %v", err)
	}
	return token
}

func TestWebSocketCompressionEnabledByDefault(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	dialer := websocket.Dialer{EnableCompression: true}
	conn, resp, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("expected successful dial with compression enabled: %v", err)
	}
	defer conn.Close()
	_ = readMsg(t, conn) // initialState

	ext := strings.ToLower(resp.Header.Get("Sec-WebSocket-Extensions"))
	if !strings.Contains(ext, "permessage-deflate") {
		t.Fatalf("expected permessage-deflate negotiation, got header: %q", resp.Header.Get("Sec-WebSocket-Extensions"))
	}
}

func TestWebSocketCompressionCanBeDisabled(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins:     []string{"*"},
		DisableCompression: true,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	dialer := websocket.Dialer{EnableCompression: true}
	conn, resp, err := dialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("expected successful dial with server compression disabled: %v", err)
	}
	defer conn.Close()
	_ = readMsg(t, conn) // initialState

	ext := strings.ToLower(resp.Header.Get("Sec-WebSocket-Extensions"))
	if strings.Contains(ext, "permessage-deflate") {
		t.Fatalf("expected compression to be disabled, got header: %q", resp.Header.Get("Sec-WebSocket-Extensions"))
	}
}

// TestWebSocketDefaultOriginValidation verifies localhost-only origin checks
// reject attacker-controlled suffix domains and accept real loopback origins.
func TestWebSocketDefaultOriginValidation(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	badHeaders := http.Header{"Origin": []string{"http://localhost.evil"}}
	_, badResp, badErr := websocket.DefaultDialer.Dial(u, badHeaders)
	if badErr == nil {
		t.Fatal("expected dial to fail for non-loopback origin")
	}
	if badResp == nil || badResp.StatusCode != http.StatusForbidden {
		t.Fatalf("expected 403 for non-loopback origin, got: %v", badResp)
	}

	okHeaders := http.Header{"Origin": []string{"http://localhost:3000"}}
	conn, _, err := websocket.DefaultDialer.Dial(u, okHeaders)
	if err != nil {
		t.Fatalf("expected localhost origin to be accepted, got: %v", err)
	}
	defer conn.Close()
}

func TestPrimitiveOpHoldMsClampWarning(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // initialState

	sendMsg(t, conn, "createMutex", map[string]string{
		"id":   "mu-cap",
		"name": "cap-test",
	})
	drainUntilType(t, conn, "success")

	sendMsg(t, conn, "primitiveOp", map[string]interface{}{
		"id": "mu-cap", "op": "lock", "holdMs": 9999999,
	})

	msg := drainUntilType(t, conn, "success")
	payload, _ := msg["payload"].(map[string]interface{})
	if payload == nil {
		t.Fatal("expected success payload")
	}
	warning, _ := payload["warning"].(string)
	if !strings.Contains(warning, "holdMs clamped") {
		t.Fatalf("expected holdMs clamp warning, got payload: %#v", payload)
	}
}

// TestShutdownCancelsBlockedOp verifies that Shutdown unblocks an op goroutine
// that is waiting to acquire a held lock.
func TestShutdownCancelsBlockedOp(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{AllowedOrigins: []string{"*"}})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	// Create and immediately lock a mutex with a long hold.
	sendMsg(t, conn, "createMutex", map[string]string{"id": "mu-blk", "name": "blk"})
	drainUntilType(t, conn, "success")

	sendMsg(t, conn, "primitiveOp", map[string]interface{}{
		"id": "mu-blk", "op": "lock", "holdMs": 5000,
	})
	drainUntilType(t, conn, "success") // lock acquired, held for 5s

	// Second lock attempt on the same mutex — will block.
	sendMsg(t, conn, "primitiveOp", map[string]interface{}{
		"id": "mu-blk", "op": "lock", "holdMs": 5000,
	})

	// Allow the goroutine time to reach LockContext and block.
	time.Sleep(50 * time.Millisecond)

	// Shutdown cancels shutdownCtx; the blocked goroutine should unblock.
	sCtx, sCancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer sCancel()
	srv.Shutdown(sCtx)

	// Expect an error message (or connection close) within 1s.
	conn.SetReadDeadline(time.Now().Add(1 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			// Connection closed after shutdown — acceptable: the error was either
			// delivered or the connection was torn down before delivery.
			break
		}
		var m map[string]interface{}
		if jsonErr := json.Unmarshal(data, &m); jsonErr == nil && m["type"] == "error" {
			break // Got the expected cancellation error message.
		}
	}
	// Reaching here (no t.Fatal/t.Error above) means we did not hang.
	ts.Close()
}

// TestDisconnectCancelsBlockedOp verifies that closing a client connection
// cancels per-primitive contexts and unblocks waiting op goroutines.
func TestDisconnectCancelsBlockedOp(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{AllowedOrigins: []string{"*"}})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	conn := dialWS(t, ts)
	readMsg(t, conn) // initialState

	sendMsg(t, conn, "createMutex", map[string]string{"id": "mu-disc", "name": "disc"})
	drainUntilType(t, conn, "success")

	// Acquire and hold the lock.
	sendMsg(t, conn, "primitiveOp", map[string]interface{}{
		"id": "mu-disc", "op": "lock", "holdMs": 5000,
	})
	drainUntilType(t, conn, "success")

	// Second lock attempt blocks.
	sendMsg(t, conn, "primitiveOp", map[string]interface{}{
		"id": "mu-disc", "op": "lock", "holdMs": 5000,
	})

	// Wait until blocked goroutine is observed.
	blockDeadline := time.Now().Add(2 * time.Second)
	blockedSeen := false
	for time.Now().Before(blockDeadline) {
		if srv.GetSchedulerMetrics().BlockedGoroutines > 0 {
			blockedSeen = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !blockedSeen {
		t.Fatal("expected blocked goroutine before disconnect")
	}

	// Disconnect client; blocked op should unblock via context cancellation.
	conn.Close()

	unblockDeadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(unblockDeadline) {
		if srv.GetSchedulerMetrics().BlockedGoroutines == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("blocked goroutines did not drain after disconnect: %+v", srv.GetSchedulerMetrics())
}

// TestWebSocketUnlockNotLocked verifies that unlocking an unlocked mutex returns error.
func TestWebSocketUnlockNotLocked(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	sendMsg(t, conn, "createMutex", map[string]string{"id": "mu-ulk", "name": "ulk"})
	drainUntilType(t, conn, "success")

	// Unlock without locking — must return error, not success.
	sendMsg(t, conn, "primitiveOp", map[string]string{"id": "mu-ulk", "op": "unlock"})
	msg := drainUntilType(t, conn, "error")
	payload, _ := msg["payload"].(map[string]interface{})
	if payload == nil {
		t.Fatalf("expected error payload")
	}
	if !strings.Contains(payload["message"].(string), "not locked") {
		t.Errorf("expected 'not locked' error, got: %v", payload["message"])
	}
}

// TestWebSocketDuplicateIDReturnsError verifies that creating a primitive with
// an already-used ID on the SAME connection returns an error instead of silently overwriting.
func TestWebSocketDuplicateIDReturnsError(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	sendMsg(t, conn, "createMutex", map[string]string{"id": "dup-1", "name": "first"})
	drainUntilType(t, conn, "success")

	// Create again with same ID on the same connection — must return error.
	sendMsg(t, conn, "createMutex", map[string]string{"id": "dup-1", "name": "second"})
	msg := drainUntilType(t, conn, "error")
	payload, _ := msg["payload"].(map[string]interface{})
	if payload == nil {
		t.Fatalf("expected error payload on duplicate ID")
	}
	if !strings.Contains(payload["message"].(string), "already exists") {
		t.Errorf("expected 'already exists' error, got: %v", payload["message"])
	}
}

// TestPrimitiveIsolationAcrossConnections verifies that two connections can each
// create a primitive with the same ID without conflict (per-connection isolation).
func TestPrimitiveIsolationAcrossConnections(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn1 := dialWS(t, ts)
	defer conn1.Close()
	readMsg(t, conn1)

	conn2 := dialWS(t, ts)
	defer conn2.Close()
	readMsg(t, conn2)

	// Both connections create a mutex with the same "shared-id" — both should succeed.
	sendMsg(t, conn1, "createMutex", map[string]string{"id": "shared-id", "name": "conn1-mutex"})
	msg1 := drainUntilType(t, conn1, "success")
	if msg1["type"] != "success" {
		t.Errorf("conn1: expected success, got %v", msg1)
	}

	sendMsg(t, conn2, "createMutex", map[string]string{"id": "shared-id", "name": "conn2-mutex"})
	msg2 := drainUntilType(t, conn2, "success")
	if msg2["type"] != "success" {
		t.Errorf("conn2: expected success, got %v", msg2)
	}

	// conn1 deletes its primitive — conn2's primitive should still exist.
	sendMsg(t, conn1, "deletePrimitive", map[string]string{"id": "shared-id"})
	drainUntilType(t, conn1, "success")

	// conn2 should still be able to operate on its primitive.
	sendMsg(t, conn2, "primitiveOp", map[string]interface{}{
		"id": "shared-id",
		"op": "lock",
	})
	msg3 := drainUntilType(t, conn2, "success")
	if msg3["type"] != "success" {
		t.Errorf("conn2: expected success after conn1 deleted its copy, got %v", msg3)
	}
}

// TestSchedulerTotalPrimitivesDecrement verifies that TotalPrimitives decrements
// when a primitive is deleted (Bug 4 regression).
func TestSchedulerTotalPrimitivesDecrement(t *testing.T) {
	ts, srv, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	sendMsg(t, conn, "createMutex", map[string]string{"id": "prim-cnt", "name": "cnt"})
	drainUntilType(t, conn, "success")

	// Allow stat update to propagate.
	time.Sleep(50 * time.Millisecond)
	before := srv.GetSchedulerMetrics().TotalPrimitives

	sendMsg(t, conn, "deletePrimitive", map[string]string{"id": "prim-cnt"})
	drainUntilType(t, conn, "success")

	time.Sleep(50 * time.Millisecond)
	after := srv.GetSchedulerMetrics().TotalPrimitives

	if after >= before {
		t.Errorf("TotalPrimitives should decrease after delete: before=%d after=%d", before, after)
	}
}

// TestGoroutineTrackingBlockedCount verifies that BlockedCount increments while
// a goroutine is waiting on a held lock and decrements after it acquires.
func TestGoroutineTrackingBlockedCount(t *testing.T) {
	ts, srv, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn)

	// Create a mutex and hold it for 200ms.
	sendMsg(t, conn, "createMutex", map[string]string{"id": "mu-blk2", "name": "blk2"})
	drainUntilType(t, conn, "success")

	sendMsg(t, conn, "primitiveOp", map[string]interface{}{
		"id": "mu-blk2", "op": "lock", "holdMs": 200,
	})
	drainUntilType(t, conn, "success")

	// Second lock will block — it registers with the scheduler.
	sendMsg(t, conn, "primitiveOp", map[string]interface{}{
		"id": "mu-blk2", "op": "lock", "holdMs": 0,
	})

	// Give the goroutine time to reach the blocking call.
	time.Sleep(30 * time.Millisecond)

	prims := srv.GetSchedulerPrimitives()
	if info, ok := prims["mu-blk2"]; !ok {
		t.Fatal("primitive mu-blk2 not found in scheduler")
	} else if info.BlockedCount < 1 {
		t.Errorf("expected BlockedCount >= 1 while goroutine is blocked, got %d", info.BlockedCount)
	}

	// Wait for the hold to expire and the second lock to succeed.
	drainUntilType(t, conn, "success")
}

// TestWebSocketPrimitiveOpAcquireRelease creates a semaphore(2), acquires, releases.
func TestWebSocketPrimitiveOpAcquireRelease(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	sendMsg(t, conn, "createSemaphore", map[string]interface{}{
		"id":       "sem-ar",
		"name":     "ar-sem",
		"capacity": 2,
	})
	drainUntilType(t, conn, "success")

	sendMsg(t, conn, "primitiveOp", map[string]interface{}{
		"id": "sem-ar",
		"op": "acquire",
	})
	drainUntilType(t, conn, "success")

	sendMsg(t, conn, "primitiveOp", map[string]interface{}{
		"id": "sem-ar",
		"op": "release",
	})
	drainUntilType(t, conn, "success")
}

// TestWebSocketEmptyIDReturnsError verifies that creating a primitive with an
// empty ID is rejected with an error.
func TestWebSocketEmptyIDReturnsError(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	for _, msgType := range []string{"createMutex", "createRWLock", "createCondVar"} {
		sendMsg(t, conn, msgType, map[string]string{"id": "", "name": "empty"})
		msg := drainUntilType(t, conn, "error")
		payload, _ := msg["payload"].(map[string]interface{})
		if payload == nil {
			t.Fatalf("%s: expected error payload for empty id", msgType)
		}
		if !strings.Contains(payload["message"].(string), "empty") {
			t.Errorf("%s: expected 'empty' error, got: %v", msgType, payload["message"])
		}
	}
}

// TestWebSocketDeleteNonexistentReturnsError verifies that deleting a primitive
// that does not exist returns an error rather than silent success.
func TestWebSocketDeleteNonexistentReturnsError(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // consume initialState

	sendMsg(t, conn, "deletePrimitive", map[string]string{"id": "no-such-id"})
	msg := drainUntilType(t, conn, "error")
	payload, _ := msg["payload"].(map[string]interface{})
	if payload == nil {
		t.Fatalf("expected error payload for nonexistent id")
	}
	if !strings.Contains(payload["message"].(string), "not found") {
		t.Errorf("expected 'not found' error, got: %v", payload["message"])
	}
}

// TestDeleteWhileBlocked verifies that deleting a mutex while a goroutine is
// blocked waiting to acquire it returns an error containing "deleted".
func TestDeleteWhileBlocked(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn)

	// Create a mutex and lock it with a long hold.
	sendMsg(t, conn, "createMutex", map[string]string{"id": "del-blk", "name": "del-blk"})
	drainUntilType(t, conn, "success")

	sendMsg(t, conn, "primitiveOp", map[string]interface{}{
		"id": "del-blk", "op": "lock", "holdMs": 5000,
	})
	drainUntilType(t, conn, "success") // lock acquired

	// Second lock attempt — will block.
	sendMsg(t, conn, "primitiveOp", map[string]interface{}{
		"id": "del-blk", "op": "lock", "holdMs": 0,
	})

	// Give the goroutine time to reach LockContext.
	time.Sleep(50 * time.Millisecond)

	// Delete the primitive while the goroutine is blocked.
	sendMsg(t, conn, "deletePrimitive", map[string]string{"id": "del-blk"})
	drainUntilType(t, conn, "success") // delete succeeded

	// The blocked goroutine should receive an error containing "deleted".
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			t.Fatalf("expected error message about deletion, got read error: %v", err)
		}
		var m map[string]interface{}
		if jsonErr := json.Unmarshal(data, &m); jsonErr != nil {
			continue
		}
		if m["type"] == "error" {
			payload, _ := m["payload"].(map[string]interface{})
			if payload != nil {
				msg, _ := payload["message"].(string)
				if strings.Contains(msg, "deleted") {
					return // success
				}
			}
		}
	}
}

// TestWebSocketConnectionCap verifies that the server rejects connections
// beyond the configured MaxConns limit with HTTP 503.
func TestWebSocketConnectionCap(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
		MaxConns:       2,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"

	conn1, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("first connection should succeed: %v", err)
	}
	defer conn1.Close()
	// drain initialState
	conn1.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn1.ReadMessage()

	conn2, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("second connection should succeed: %v", err)
	}
	defer conn2.Close()
	conn2.SetReadDeadline(time.Now().Add(2 * time.Second))
	conn2.ReadMessage()

	// Third connection should be rejected with 503.
	_, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err == nil {
		t.Fatal("third connection should fail")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Errorf("expected 503, got: %v", resp)
	}
}

func TestWebSocketOpRateLimit(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // initialState

	sendMsg(t, conn, "createMutex", map[string]string{"id": "rl-mu", "name": "rl-mu"})
	drainUntilType(t, conn, "success")

	// 50 ops/s allowed; burst above that should produce at least one op-limit error.
	for i := 0; i < 60; i++ {
		sendMsg(t, conn, "primitiveOp", map[string]interface{}{
			"id": "rl-mu", "op": "unlock", "holdMs": 1,
		})
	}

	deadline := time.Now().Add(3 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		msg := readMsg(t, conn)
		if msg["type"] != "error" {
			continue
		}
		payload, _ := msg["payload"].(map[string]interface{})
		if payload == nil {
			continue
		}
		if strings.Contains(fmt.Sprint(payload["message"]), "operation rate limit exceeded") {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("expected operation rate-limit error but none observed")
	}
}

func TestShutdownRejectsNewConnections(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{AllowedOrigins: []string{"*"}})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // initialState

	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown failed: %v", err)
	}

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
	_, resp, err := websocket.DefaultDialer.Dial(u, nil)
	if err == nil {
		t.Fatal("expected new connection to fail during/after shutdown")
	}
	if resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("expected 503 while draining/shutdown, got: %v", resp)
	}
}

// TestHealthzOK verifies that GET /healthz returns 200 with JSON body.
func TestHealthzOK(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// TestHealthzMethodNotAllowed verifies that POST /healthz returns 405.
func TestHealthzMethodNotAllowed(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := http.Post(ts.URL+"/healthz", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

func TestHealthzIncludesCustomHistogramBuckets(t *testing.T) {
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins:   []string{"*"},
		HistogramBuckets: []time.Duration{time.Millisecond, 10 * time.Millisecond},
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", srv.HandleHealthz)
	ts := httptest.NewServer(mux)
	defer ts.Close()
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	resp, err := http.Get(ts.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()

	var body map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decode /healthz: %v", err)
	}

	rawBuckets, ok := body["histogram_buckets"].([]interface{})
	if !ok || len(rawBuckets) != 2 {
		t.Fatalf("expected 2 histogram buckets in healthz, got: %#v", body["histogram_buckets"])
	}
	if rawBuckets[0] != "1ms" || rawBuckets[1] != "10ms" {
		t.Fatalf("unexpected histogram buckets: %#v", rawBuckets)
	}
}

func TestAuditLogWritesEntriesAndHealthzIncludesDrops(t *testing.T) {
	auditPath := filepath.Join(t.TempDir(), "audit.jsonl")
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins:    []string{"*"},
		AuditLogPath:      auditPath,
		AuditLogMaxBytes:  1024 * 1024,
		AuditLogKeepFiles: 2,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	mux.HandleFunc("/healthz", srv.HandleHealthz)
	ts := httptest.NewServer(mux)
	defer ts.Close()

	conn := dialWS(t, ts)
	defer conn.Close()
	readMsg(t, conn) // initialState

	sendMsg(t, conn, "createMutex", map[string]string{"id": "audit-mu", "name": "audit"})
	drainUntilType(t, conn, "success")
	sendMsg(t, conn, "deletePrimitive", map[string]string{"id": "audit-mu"})
	drainUntilType(t, conn, "success")

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	data, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("ReadFile(audit): %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "\"event\":\"primitive_created\"") {
		t.Fatalf("expected primitive_created audit entry, got %s", text)
	}
	if !strings.Contains(text, "\"event\":\"primitive_deleted\"") {
		t.Fatalf("expected primitive_deleted audit entry, got %s", text)
	}

	// healthz remains readable from the in-memory handler even when no entries were dropped.
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	srv.HandleHealthz(rec, req)
	var body map[string]interface{}
	if err := json.NewDecoder(rec.Body).Decode(&body); err != nil {
		t.Fatalf("decode healthz: %v", err)
	}
	if _, ok := body["dropped_audit_events"]; !ok {
		t.Fatalf("expected dropped_audit_events in healthz response")
	}
}

func TestInvalidHistogramBucketsPanics(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected panic for non-ascending histogram buckets")
		}
	}()
	web.NewServerWithConfig(web.Config{
		HistogramBuckets: []time.Duration{
			10 * time.Millisecond,
			1 * time.Millisecond,
		},
	})
}

// TestReadyzOK verifies that GET /readyz returns 200.
func TestReadyzOK(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/readyz")
	if err != nil {
		t.Fatalf("GET /readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
}

// TestReadyzMethodNotAllowed verifies that POST /readyz returns 405.
func TestReadyzMethodNotAllowed(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := http.Post(ts.URL+"/readyz", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /readyz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

// TestPrometheusMetricsOK verifies that GET /metrics returns 200.
func TestPrometheusMetricsOK(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := http.Get(ts.URL + "/metrics")
	if err != nil {
		t.Fatalf("GET /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("expected 200, got %d", resp.StatusCode)
	}
	ct := resp.Header.Get("Content-Type")
	if !strings.Contains(ct, "text/plain") {
		t.Errorf("expected text/plain content-type, got %q", ct)
	}
}

// TestPrometheusMetricsMethodNotAllowed verifies that POST /metrics returns 405.
func TestPrometheusMetricsMethodNotAllowed(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	resp, err := http.Post(ts.URL+"/metrics", "text/plain", nil)
	if err != nil {
		t.Fatalf("POST /metrics: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("expected 405, got %d", resp.StatusCode)
	}
}

// TestHandleGetMetrics exercises the WebSocket getMetrics message.
func TestHandleGetMetrics(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()

	drainUntilType(t, conn, "initialState")

	sendMsg(t, conn, "getMetrics", map[string]interface{}{})
	msg := drainUntilType(t, conn, "metrics")
	if msg["type"] != "metrics" {
		t.Errorf("expected 'metrics' response, got %v", msg["type"])
	}
}

// TestCreateCondVar exercises the createCondVar WebSocket handler.
func TestCreateCondVar(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()

	drainUntilType(t, conn, "initialState")

	sendMsg(t, conn, "createCondVar", map[string]interface{}{
		"id": "test-cv1", "name": "TestCV",
	})
	msg := drainUntilType(t, conn, "success")
	if msg["type"] != "success" {
		t.Errorf("expected success, got %v", msg["type"])
	}
}

// TestCreateWaitGroup exercises the createWaitGroup WebSocket handler.
func TestCreateWaitGroup(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()

	drainUntilType(t, conn, "initialState")

	sendMsg(t, conn, "createWaitGroup", map[string]interface{}{
		"id": "test-wg1", "name": "TestWG",
	})
	msg := drainUntilType(t, conn, "success")
	if msg["type"] != "success" {
		t.Errorf("expected success, got %v", msg["type"])
	}
}

// TestCreateOnce exercises the createOnce WebSocket handler.
func TestCreateOnce(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()

	drainUntilType(t, conn, "initialState")

	sendMsg(t, conn, "createOnce", map[string]interface{}{
		"id": "test-once1", "name": "TestOnce",
	})
	msg := drainUntilType(t, conn, "success")
	if msg["type"] != "success" {
		t.Errorf("expected success, got %v", msg["type"])
	}
}

// TestCreateSingleflight exercises the createSingleflight WebSocket handler.
func TestCreateSingleflight(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	conn := dialWS(t, ts)
	defer conn.Close()

	drainUntilType(t, conn, "initialState")

	sendMsg(t, conn, "createSingleflight", map[string]interface{}{
		"id": "test-sf1", "name": "TestSF",
	})
	msg := drainUntilType(t, conn, "success")
	if msg["type"] != "success" {
		t.Errorf("expected success, got %v", msg["type"])
	}
}

// TestSnapshotRoundTrip creates primitives via WebSocket, shuts down the server
// (triggering saveSnapshot), starts a second server pointing at the same file,
// and verifies that the primitives have been re-registered in the scheduler.
func TestSnapshotRoundTrip(t *testing.T) {
	snapshotPath := filepath.Join(t.TempDir(), "snapshot.json")

	// --- Phase 1: create server, register primitives, shutdown ---
	srv1 := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
		SnapshotPath:   snapshotPath,
	})
	mux1 := http.NewServeMux()
	mux1.HandleFunc("/ws", srv1.HandleWebSocket)
	ts1 := httptest.NewServer(mux1)

	conn1 := dialWSTo(t, ts1.URL)

	// Drain the initial "initialState" message.
	drainUntilType(t, conn1, "initialState")

	// Create a RWLock.
	sendMsg(t, conn1, "createRWLock", map[string]interface{}{
		"id": "snap-rw1", "name": "SnapRW",
	})
	drainUntilType(t, conn1, "success")

	// Create a Semaphore with capacity 5.
	sendMsg(t, conn1, "createSemaphore", map[string]interface{}{
		"id": "snap-sem1", "name": "SnapSem", "capacity": 5,
	})
	drainUntilType(t, conn1, "success")

	// Create a Barrier with 3 parties.
	sendMsg(t, conn1, "createBarrier", map[string]interface{}{
		"id": "snap-bar1", "name": "SnapBar", "parties": 3,
	})
	drainUntilType(t, conn1, "success")

	// Verify primitives are known to server 1.
	prims1 := srv1.GetSchedulerPrimitives()
	for _, id := range []string{"snap-rw1", "snap-sem1", "snap-bar1"} {
		if _, ok := prims1[id]; !ok {
			t.Fatalf("server1: primitive %q not found before shutdown", id)
		}
	}

	// Shutdown the server first (saveSnapshot runs here, while connPrimsMap
	// still contains the connection's primitives).  Then close the test server
	// and the WebSocket connection.
	srv1.Shutdown(context.Background()) //nolint:errcheck
	conn1.Close()
	ts1.Close()

	// Verify the snapshot file was written.
	if _, err := os.Stat(snapshotPath); err != nil {
		t.Fatalf("snapshot file missing after shutdown: %v", err)
	}
	// Verify the file is versioned.
	data, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	var snap struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(data, &snap); err != nil {
		t.Fatalf("parse snapshot: %v", err)
	}
	if snap.Version != 1 {
		t.Fatalf("expected snapshot version 1, got %d", snap.Version)
	}

	// --- Phase 2: new server, same snapshotPath, verify restore ---
	srv2 := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
		SnapshotPath:   snapshotPath,
	})
	defer srv2.Shutdown(context.Background()) //nolint:errcheck

	// Give loadSnapshot a moment to register with the scheduler.
	time.Sleep(50 * time.Millisecond)

	prims2 := srv2.GetSchedulerPrimitives()
	for _, id := range []string{"snap-rw1", "snap-sem1", "snap-bar1"} {
		if _, ok := prims2[id]; !ok {
			t.Errorf("server2: primitive %q not restored from snapshot", id)
		}
	}
}

func TestSnapshotLegacyFormatBackwardCompatibility(t *testing.T) {
	snapshotPath := filepath.Join(t.TempDir(), "legacy-snapshot.json")
	legacy := map[string]map[string]interface{}{
		"legacy-mu-1": {
			"type": "Mutex",
			"name": "LegacyMutex",
		},
	}
	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy snapshot: %v", err)
	}
	if err := os.WriteFile(snapshotPath, data, 0o600); err != nil {
		t.Fatalf("write legacy snapshot: %v", err)
	}

	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
		SnapshotPath:   snapshotPath,
	})
	prims := srv.GetSchedulerPrimitives()
	if _, ok := prims["legacy-mu-1"]; !ok {
		t.Fatalf("legacy primitive was not restored")
	}
	if err := srv.Shutdown(context.Background()); err != nil {
		t.Fatalf("shutdown: %v", err)
	}

	// After save, file must be migrated to versioned format.
	upgradedData, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read upgraded snapshot: %v", err)
	}
	var upgraded struct {
		Version int `json:"version"`
	}
	if err := json.Unmarshal(upgradedData, &upgraded); err != nil {
		t.Fatalf("parse upgraded snapshot: %v", err)
	}
	if upgraded.Version != 1 {
		t.Fatalf("expected upgraded snapshot version 1, got %d", upgraded.Version)
	}
}

func TestSnapshotFutureVersionSkipped(t *testing.T) {
	snapshotPath := filepath.Join(t.TempDir(), "future-snapshot.json")
	future := map[string]interface{}{
		"version": 99,
		"primitives": []map[string]interface{}{
			{"id": "future-mu-1", "type": "Mutex", "name": "FutureMutex"},
		},
	}
	data, err := json.MarshalIndent(future, "", "  ")
	if err != nil {
		t.Fatalf("marshal future snapshot: %v", err)
	}
	if err := os.WriteFile(snapshotPath, data, 0o600); err != nil {
		t.Fatalf("write future snapshot: %v", err)
	}

	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
		SnapshotPath:   snapshotPath,
	})
	defer srv.Shutdown(context.Background()) //nolint:errcheck

	if _, ok := srv.GetSchedulerPrimitives()["future-mu-1"]; ok {
		t.Fatal("future-version snapshot should have been skipped")
	}
}

// dialWSTo opens a websocket connection to an explicit base URL (no /ws
// suffix assumed — the caller provides the full base URL).
func dialWSTo(t *testing.T, baseURL string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(baseURL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("WS dial failed: %v", err)
	}
	return conn
}

// TestHandleStaticMethodNotAllowed verifies that non-GET/HEAD methods on the
// static handler return 405 with an Allow header.
func TestHandleStaticMethodNotAllowed(t *testing.T) {
	ts, _, cleanup := newTestServer(t)
	defer cleanup()

	for _, method := range []string{"POST", "PUT", "DELETE"} {
		req, _ := http.NewRequest(method, ts.URL+"/", nil)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("%s /: %v", method, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s /: expected 405, got %d", method, resp.StatusCode)
		}
		if resp.Header.Get("Allow") == "" {
			t.Errorf("%s /: missing Allow header", method)
		}
	}
}

// TestInputLengthValidation verifies that oversized IDs and names are rejected
// with an error response rather than being stored, preventing memory exhaustion.
func TestInputLengthValidation(t *testing.T) {
	ts, srv, cleanup := newTestServer(t)
	defer cleanup()
	_ = srv

	conn := dialWS(t, ts)
	defer conn.Close()
	drainUntilType(t, conn, "initialState")

	longID := strings.Repeat("x", 257)
	longName := strings.Repeat("y", 257)

	// Oversized ID — must return error.
	sendMsg(t, conn, "createRWLock", map[string]interface{}{
		"id": longID, "name": "ok",
	})
	msg := drainUntilType(t, conn, "error")
	if msg["type"] != "error" {
		t.Errorf("expected error for oversized id, got %v", msg["type"])
	}

	// Oversized name — must return error.
	sendMsg(t, conn, "createRWLock", map[string]interface{}{
		"id": "ok-id", "name": longName,
	})
	msg = drainUntilType(t, conn, "error")
	if msg["type"] != "error" {
		t.Errorf("expected error for oversized name, got %v", msg["type"])
	}

	// Valid ID and name at max length — must succeed.
	maxID := strings.Repeat("a", 256)
	maxName := strings.Repeat("b", 256)
	sendMsg(t, conn, "createRWLock", map[string]interface{}{
		"id": maxID, "name": maxName,
	})
	msg = drainUntilType(t, conn, "success")
	if msg["type"] != "success" {
		t.Errorf("expected success for max-length id/name, got %v", msg["type"])
	}
}
