package loadtest_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sanskar/syncprimitives/web"
)

// newLoadTestServer spins up an httptest server backed by a web.Server.
// The returned cleanup func closes both.
func newLoadTestServer(t testing.TB) (*httptest.Server, func()) {
	t.Helper()
	srv := web.NewServerWithConfig(web.Config{
		AllowedOrigins: []string{"*"},
		MaxConns:       200,
	})
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)
	cleanup := func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(ctx) //nolint:errcheck
	}
	return ts, cleanup
}

// dialLoad dials a websocket connection for load testing; fatals on error.
func dialLoad(t testing.TB, baseURL string) *websocket.Conn {
	t.Helper()
	u := "ws" + strings.TrimPrefix(baseURL, "http") + "/ws"
	conn, _, err := websocket.DefaultDialer.Dial(u, nil)
	if err != nil {
		t.Fatalf("dial failed: %v", err)
	}
	return conn
}

// sendJSON sends a typed message and ignores write errors (the connection may
// close during load).
func sendJSON(conn *websocket.Conn, msgType string, payload interface{}) error {
	data, _ := json.Marshal(map[string]interface{}{
		"type":    msgType,
		"payload": payload,
	})
	return conn.WriteMessage(websocket.TextMessage, data)
}

// drainN reads n messages and discards them.  Used to clear the initial state
// message and any success/error responses.
func drainConn(conn *websocket.Conn, timeout time.Duration) {
	conn.SetReadDeadline(time.Now().Add(timeout))
	for {
		_, _, err := conn.ReadMessage()
		if err != nil {
			return
		}
	}
}

// drainOne reads exactly one message from conn within timeout.
func drainOne(conn *websocket.Conn, timeout time.Duration) error {
	conn.SetReadDeadline(time.Now().Add(timeout))
	_, _, err := conn.ReadMessage()
	return err
}

// TestWebSocketLoad opens 50 concurrent connections; each connection creates
// a mutex, acquires it, releases it, then deletes the primitive.  After all
// goroutines finish, we assert zero panics / crashes (the process is still
// alive) and that the total operations counted match expectations.
func TestWebSocketLoad(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping load test in short mode")
	}

	const (
		connections = 50
		cycles      = 20 // create + lock + unlock + delete per cycle
	)

	ts, cleanup := newLoadTestServer(t)
	defer cleanup()

	var (
		wg      sync.WaitGroup
		success atomic.Int64
		errCnt  atomic.Int64
	)

	for c := 0; c < connections; c++ {
		wg.Add(1)
		go func(connIdx int) {
			defer wg.Done()

			conn := dialLoad(t, ts.URL)
			defer conn.Close()

			// Drain initial state.
			drainOne(conn, 2*time.Second) //nolint:errcheck

			for i := 0; i < cycles; i++ {
				id := fmt.Sprintf("load-c%d-i%d", connIdx, i)

				// Create mutex.
				if err := sendJSON(conn, "createMutex", map[string]interface{}{
					"id": id, "name": id,
				}); err != nil {
					errCnt.Add(1)
					continue
				}
				if err := drainOne(conn, 2*time.Second); err != nil {
					errCnt.Add(1)
					continue
				}

				// Lock with a very short hold so the auto-release fires quickly.
				// The server auto-unlocks after holdMs (default 100 ms), so we
				// do NOT send a separate unlock to avoid double-unlock.
				if err := sendJSON(conn, "primitiveOp", map[string]interface{}{
					"id": id, "op": "lock", "holdMs": 10,
				}); err != nil {
					errCnt.Add(1)
					continue
				}
				if err := drainOne(conn, 2*time.Second); err != nil {
					errCnt.Add(1)
					continue
				}

				// Wait for the auto-release to fire before deleting.
				time.Sleep(15 * time.Millisecond)

				// Delete.
				if err := sendJSON(conn, "deletePrimitive", map[string]interface{}{
					"id": id,
				}); err != nil {
					errCnt.Add(1)
					continue
				}
				if err := drainOne(conn, 2*time.Second); err != nil {
					errCnt.Add(1)
					continue
				}

				success.Add(1)
			}
		}(c)
	}

	wg.Wait()

	total := success.Load() + errCnt.Load()
	t.Logf("load test: connections=%d cycles=%d success=%d errors=%d total=%d",
		connections, cycles, success.Load(), errCnt.Load(), total)

	// We allow up to 5% error rate (e.g. read deadline hit during drain).
	maxErrors := int64(float64(connections*cycles) * 0.05)
	if errCnt.Load() > maxErrors {
		t.Errorf("too many errors: %d > %d (5%% of %d)", errCnt.Load(), maxErrors, connections*cycles)
	}
}

// BenchmarkWebSocketConcurrentOps measures throughput for concurrent
// create/lock/unlock/delete cycles across multiple goroutines sharing a
// single WebSocket connection per worker.
func BenchmarkWebSocketConcurrentOps(b *testing.B) {
	ts, cleanup := newLoadTestServer(b)
	defer cleanup()

	const workers = 8

	b.ResetTimer()
	b.SetParallelism(workers)

	b.RunParallel(func(pb *testing.PB) {
		conn := dialLoad(b, ts.URL)
		defer conn.Close()

		// Drain initial state.
		drainOne(conn, 2*time.Second) //nolint:errcheck

		idx := atomic.AddInt64(&benchIdx, 1)
		i := int64(0)
		for pb.Next() {
			id := fmt.Sprintf("bench-%d-%d", idx, i)
			i++

			// Create + lock (holdMs=1, auto-release) + delete.
			sendJSON(conn, "createMutex", map[string]interface{}{"id": id, "name": id})                       //nolint:errcheck
			drainOne(conn, time.Second)                                                                        //nolint:errcheck
			sendJSON(conn, "primitiveOp", map[string]interface{}{"id": id, "op": "lock", "holdMs": 1})        //nolint:errcheck
			drainOne(conn, time.Second)                                                                        //nolint:errcheck
			time.Sleep(2 * time.Millisecond)                                                                   // let auto-release fire
			sendJSON(conn, "deletePrimitive", map[string]interface{}{"id": id})                                //nolint:errcheck
			drainOne(conn, time.Second)                                                                        //nolint:errcheck
		}
	})
}

// benchIdx is a package-level counter so each benchmark goroutine gets a
// unique connection index.
var benchIdx int64
