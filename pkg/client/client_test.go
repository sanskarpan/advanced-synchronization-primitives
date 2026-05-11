package client_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	client "github.com/sanskar/syncprimitives/pkg/client"
	"github.com/sanskar/syncprimitives/web"
)

func newClientServer(t *testing.T, cfg web.Config) (*httptest.Server, func()) {
	t.Helper()
	srv := web.NewServerWithConfig(cfg)

	mux := http.NewServeMux()
	mux.HandleFunc("/ws", srv.HandleWebSocket)
	ts := httptest.NewServer(mux)

	cleanup := func() {
		ts.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}
	return ts, cleanup
}

func wsURL(ts *httptest.Server) string {
	return "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws"
}

func newConnectedClient(t *testing.T, ts *httptest.Server, apiKey string) *client.Client {
	t.Helper()
	c := client.New(client.Config{
		URL:    wsURL(ts),
		APIKey: apiKey,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func TestClientCreateAndDeleteMutex(t *testing.T) {
	ts, cleanup := newClientServer(t, web.Config{AllowedOrigins: []string{"*"}})
	defer cleanup()

	c := newConnectedClient(t, ts, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.CreateMutex(ctx, "m1", "mutex-1"); err != nil {
		t.Fatalf("CreateMutex: %v", err)
	}
	if err := c.DeletePrimitive(ctx, "m1"); err != nil {
		t.Fatalf("DeletePrimitive: %v", err)
	}
}

func TestClientLockUnlockFlow(t *testing.T) {
	ts, cleanup := newClientServer(t, web.Config{AllowedOrigins: []string{"*"}})
	defer cleanup()

	c := newConnectedClient(t, ts, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.CreateMutex(ctx, "m2", "mutex-2"); err != nil {
		t.Fatalf("CreateMutex: %v", err)
	}
	if err := c.LockMutex(ctx, "m2", 5); err != nil {
		t.Fatalf("LockMutex: %v", err)
	}
	time.Sleep(10 * time.Millisecond)
	if err := c.DeletePrimitive(ctx, "m2"); err != nil {
		t.Fatalf("DeletePrimitive: %v", err)
	}
}

func TestClientContextCancellation(t *testing.T) {
	ts, cleanup := newClientServer(t, web.Config{AllowedOrigins: []string{"*"}})
	defer cleanup()

	c := newConnectedClient(t, ts, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.CreateBarrier(ctx, "bar-1", "barrier", 2); err != nil {
		t.Fatalf("CreateBarrier: %v", err)
	}

	waitCtx, waitCancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer waitCancel()
	err := c.WaitBarrier(waitCtx, "bar-1")
	if err == nil {
		t.Fatal("expected WaitBarrier to fail on context cancellation")
	}
}

func TestClientConcurrentOperations(t *testing.T) {
	ts, cleanup := newClientServer(t, web.Config{AllowedOrigins: []string{"*"}})
	defer cleanup()

	const workers = 6
	var wg sync.WaitGroup
	wg.Add(workers)

	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			c := newConnectedClient(t, ts, "")
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()

			id := "sem-" + string(rune('a'+i))
			if err := c.CreateSemaphore(ctx, id, id, 1); err != nil {
				t.Errorf("CreateSemaphore(%s): %v", id, err)
				return
			}
			if err := c.AcquireSemaphore(ctx, id, 1); err != nil {
				t.Errorf("AcquireSemaphore(%s): %v", id, err)
				return
			}
			time.Sleep(3 * time.Millisecond)
			if err := c.DeletePrimitive(ctx, id); err != nil {
				t.Errorf("DeletePrimitive(%s): %v", id, err)
			}
		}()
	}

	wg.Wait()
}

func TestClientAuthHeader(t *testing.T) {
	ts, cleanup := newClientServer(t, web.Config{
		AllowedOrigins: []string{"*"},
		APIKey:         "secret123",
	})
	defer cleanup()

	c := client.New(client.Config{
		URL:    wsURL(ts),
		APIKey: "secret123",
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect with API key: %v", err)
	}
	_ = c.Close()
}

func TestClientIgnoresBroadcastsWhileWaitingForAck(t *testing.T) {
	ts, cleanup := newClientServer(t, web.Config{AllowedOrigins: []string{"*"}})
	defer cleanup()

	c := newConnectedClient(t, ts, "")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := c.CreateMutex(ctx, "m-broadcast", "mutex-broadcast"); err != nil {
		t.Fatalf("CreateMutex: %v", err)
	}

	time.Sleep(1200 * time.Millisecond)

	if err := c.DeletePrimitive(ctx, "m-broadcast"); err != nil {
		t.Fatalf("DeletePrimitive after broadcast noise: %v", err)
	}
}

func TestClientAutoReconnect(t *testing.T) {
	ts, cleanup := newClientServer(t, web.Config{AllowedOrigins: []string{"*"}})
	defer cleanup()

	c := client.New(client.Config{
		URL:           wsURL(ts),
		AutoReconnect: true,
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := c.Connect(ctx); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })

	if err := c.CreateMutex(ctx, "auto-reconnect", "auto-reconnect"); err != nil {
		t.Fatalf("CreateMutex: %v", err)
	}

	_ = c.Close()

	c = client.New(client.Config{
		URL:           wsURL(ts),
		AutoReconnect: true,
	})
	if err := c.CreateMutex(ctx, "auto-reconnect-2", "auto-reconnect-2"); err != nil {
		t.Fatalf("CreateMutex with auto reconnect: %v", err)
	}
	_ = c.Close()
}
