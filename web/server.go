package web

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"github.com/sanskar/syncprimitives/internal/metrics"
	"github.com/sanskar/syncprimitives/internal/primitives"
	"github.com/sanskar/syncprimitives/internal/scheduler"
)

//go:embed static
var staticFS embed.FS

// Config holds server configuration options.
type Config struct {
	// AllowedOrigins is the list of permitted WebSocket origins.
	// Empty slice = localhost only; ["*"] = allow all origins.
	AllowedOrigins []string

	// TLSCertFile and TLSKeyFile enable TLS when both are non-empty.
	TLSCertFile string
	TLSKeyFile  string

	// APIKey, when non-empty, requires WebSocket clients to authenticate via
	// the "Authorization: Bearer <key>" request header.
	// URL query parameters are NOT accepted to prevent credential leakage in
	// server logs and browser history.
	APIKey string

	// MaxConns is the maximum number of concurrent WebSocket connections.
	// 0 means use the default of 1000.
	MaxConns int

	// SnapshotPath is the file used to persist primitive state across restarts.
	// Empty string disables persistence.
	SnapshotPath string

	// DisableCompression disables permessage-deflate compression for WebSocket
	// messages. Compression is enabled by default.
	DisableCompression bool
}

// connState holds per-connection rate-limiting state.
type connState struct {
	// Sliding-window rate limit: track message timestamps.
	msgTimes []time.Time
	opTimes  []time.Time
	mu       sync.Mutex
}

// deltaState tracks per-connection snapshots for delta broadcasts.
type deltaState struct {
	lastPrimJSON      map[string]string
	lastFullRefreshAt time.Time
}

// primEntry holds a primitive's context alongside its cancel function.
type primEntry struct {
	ctx    context.Context
	cancel context.CancelFunc
}

// connPrims holds all synchronization primitives owned by a single WebSocket
// connection. Isolation between connections is achieved by storing one
// connPrims per conn instead of global maps on Server.
type connPrims struct {
	mu            sync.RWMutex
	rwlocks       map[string]*primitives.RWLock
	semaphores    map[string]*primitives.Semaphore
	mutexes       map[string]*primitives.Mutex
	condvars      map[string]*primitives.CondVar
	barriers      map[string]*primitives.Barrier
	waitgroups    map[string]*primitives.WaitGroup
	onces         map[string]*primitives.Once
	singleflights map[string]*primitives.Group

	// primCtxs stores a ctx+cancel per primitive so that in-flight blocking
	// operations can be cancelled when the primitive is deleted (Fix 4).
	primCtxsMu sync.Mutex
	primCtxs   map[string]primEntry
}

// newConnPrims allocates an empty per-connection primitives container.
func newConnPrims() *connPrims {
	return &connPrims{
		rwlocks:       make(map[string]*primitives.RWLock),
		semaphores:    make(map[string]*primitives.Semaphore),
		mutexes:       make(map[string]*primitives.Mutex),
		condvars:      make(map[string]*primitives.CondVar),
		barriers:      make(map[string]*primitives.Barrier),
		waitgroups:    make(map[string]*primitives.WaitGroup),
		onces:         make(map[string]*primitives.Once),
		singleflights: make(map[string]*primitives.Group),
		primCtxs:      make(map[string]primEntry),
	}
}

// primExists reports whether id is registered in any primitive map.
// Must be called with cp.mu held (read or write).
func (cp *connPrims) primExists(id string) bool {
	_, ok1 := cp.rwlocks[id]
	_, ok2 := cp.semaphores[id]
	_, ok3 := cp.mutexes[id]
	_, ok4 := cp.condvars[id]
	_, ok5 := cp.barriers[id]
	_, ok6 := cp.waitgroups[id]
	_, ok7 := cp.onces[id]
	_, ok8 := cp.singleflights[id]
	return ok1 || ok2 || ok3 || ok4 || ok5 || ok6 || ok7 || ok8
}

// Server manages WebSocket connections and the synchronization primitives
type Server struct {
	cfg              Config
	scheduler        *scheduler.Scheduler
	metricsCollector *metrics.MetricsCollector

	// Per-connection primitive maps (Fix 3).
	connPrimsMap map[*websocket.Conn]*connPrims
	connPrimsMu  sync.RWMutex

	// WebSocket clients
	clients   map[*websocket.Conn]bool
	clientsMu sync.RWMutex
	writeMu   map[*websocket.Conn]*sync.Mutex
	writeMuMu sync.Mutex

	// Per-connection rate-limit state
	connStates   map[*websocket.Conn]*connState
	connStatesMu sync.Mutex

	// Per-connection delta-broadcast state.
	deltaStates   map[*websocket.Conn]*deltaState
	deltaStatesMu sync.Mutex

	broadcast chan interface{}

	// droppedBroadcasts counts how many broadcast sends were dropped due to
	// a full channel. Increment and log when the broadcast channel is full.
	droppedBroadcasts atomic.Int64
	// rate-limit counters for observability.
	msgRateLimitHits    atomic.Int64
	opRateLimitHits     atomic.Int64
	fullRefreshRequests atomic.Int64
	updateSequence      atomic.Uint64

	// Connection cap
	maxConns    int
	activeConns atomic.Int64
	draining    atomic.Bool

	// httpServer is stored so Shutdown can drain open connections.
	httpServer *http.Server
	// stop is closed when Shutdown is called; background goroutines exit on it.
	stop chan struct{}

	// shutdownCtx is cancelled by Shutdown to unblock goroutines waiting on
	// context-aware primitives (RLockContext, LockContext, AcquireContext, etc.).
	shutdownCtx    context.Context
	shutdownCancel context.CancelFunc

	// goroutineCounter is incremented for each op goroutine to produce a
	// unique ID for scheduler goroutine tracking.
	goroutineCounter atomic.Uint64

	// snapshotPath is the file path used for JSON state persistence (Fix 8).
	snapshotPath string
}

// Message represents a WebSocket message
type Message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

// upgraderFor returns a websocket.Upgrader configured with the given allowed origins.
func upgraderFor(cfg Config) websocket.Upgrader {
	return websocket.Upgrader{
		ReadBufferSize:    1024,
		WriteBufferSize:   1024,
		EnableCompression: !cfg.DisableCompression,
		CheckOrigin: func(r *http.Request) bool {
			if len(cfg.AllowedOrigins) == 0 {
				// Default: localhost only
				origin := r.Header.Get("Origin")
				if origin == "" {
					return true
				}
				return isLoopbackOrigin(origin)
			}
			for _, allowed := range cfg.AllowedOrigins {
				if allowed == "*" {
					return true
				}
				origin := r.Header.Get("Origin")
				if origin == allowed {
					return true
				}
			}
			return false
		},
	}
}

func isLoopbackOrigin(origin string) bool {
	u, err := url.Parse(origin)
	if err != nil {
		return false
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return false
	}
	host := u.Hostname()
	if host == "" {
		host = u.Host
	}
	if host == "localhost" || host == "127.0.0.1" || host == "::1" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// NewServer creates a new web server with default (localhost-only) config.
func NewServer() *Server {
	return NewServerWithConfig(Config{})
}

// NewServerWithConfig creates a new web server with the given Config.
func NewServerWithConfig(cfg Config) *Server {
	ctx, cancel := context.WithCancel(context.Background())
	maxConns := cfg.MaxConns
	if maxConns <= 0 {
		maxConns = 1000
	}
	snapshotPath := cfg.SnapshotPath
	s := &Server{
		cfg:              cfg,
		scheduler:        scheduler.NewScheduler(),
		metricsCollector: metrics.NewMetricsCollector(),
		connPrimsMap:     make(map[*websocket.Conn]*connPrims),
		clients:          make(map[*websocket.Conn]bool),
		writeMu:          make(map[*websocket.Conn]*sync.Mutex),
		connStates:       make(map[*websocket.Conn]*connState),
		deltaStates:      make(map[*websocket.Conn]*deltaState),
		broadcast:        make(chan interface{}, 100),
		stop:             make(chan struct{}),
		shutdownCtx:      ctx,
		shutdownCancel:   cancel,
		maxConns:         maxConns,
		snapshotPath:     snapshotPath,
	}

	s.scheduler.Start()

	go s.broadcastMessages()
	go s.forwardSchedulerUpdates()
	go s.updatePrimitiveStats()

	// Restore primitives persisted from the previous run (Fix 8).
	s.loadSnapshot()

	return s
}

// HandleWebSocket handles WebSocket connections
func (s *Server) HandleWebSocket(w http.ResponseWriter, r *http.Request) {
	// A1: API key authentication — checked before upgrade so we can return 401.
	// Only the Authorization: Bearer <key> header is accepted.  URL query
	// parameters are intentionally NOT supported: they appear in server access
	// logs and browser history, creating a credential-leakage risk.
	if s.cfg.APIKey != "" {
		auth := r.Header.Get("Authorization")
		key := strings.TrimPrefix(auth, "Bearer ")
		if key == "" || key == auth || key != s.cfg.APIKey {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
	}
	if s.draining.Load() {
		http.Error(w, "Server is shutting down", http.StatusServiceUnavailable)
		return
	}

	// Check connection cap before upgrading.
	if s.activeConns.Load() >= int64(s.maxConns) {
		http.Error(w, "Service Unavailable: too many connections", http.StatusServiceUnavailable)
		return
	}

	upgrader := upgraderFor(s.cfg)
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		slog.Error("WebSocket upgrade failed", "err", err)
		return
	}
	s.activeConns.Add(1)

	// R2: limit max message size to 64 KiB
	conn.SetReadLimit(64 * 1024)

	s.registerClient(conn)
	defer s.unregisterClient(conn)

	// R1: start ping/pong keepalive goroutine
	go s.pingLoop(conn)

	// Send initial state
	s.sendInitialState(conn)

	// Handle incoming messages
	for {
		var msg Message
		err := conn.ReadJSON(&msg)
		if err != nil {
			if websocket.IsUnexpectedCloseError(err, websocket.CloseGoingAway, websocket.CloseAbnormalClosure) {
				slog.Error("WebSocket error", "err", err)
			}
			break
		}

		// S3: per-connection rate limiting (max 200 msg/s sliding window)
		if !s.checkRateLimit(conn) {
			s.msgRateLimitHits.Add(1)
			s.sendError(conn, "rate limit exceeded: max 200 messages/second")
			continue
		}
		if msg.Type == "primitiveOp" && !s.checkOpRateLimit(conn) {
			s.opRateLimitHits.Add(1)
			s.sendError(conn, "operation rate limit exceeded: max 50 ops/second per connection")
			continue
		}

		s.handleMessage(conn, msg)
	}
}

// checkRateLimit returns true if the connection is within the rate limit.
// Uses a sliding 1-second window of up to 200 messages.
func (s *Server) checkRateLimit(conn *websocket.Conn) bool {
	const maxMsgsPerSec = 200
	now := time.Now()
	cutoff := now.Add(-time.Second)

	s.connStatesMu.Lock()
	cs, ok := s.connStates[conn]
	s.connStatesMu.Unlock()
	if !ok {
		return true
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	// Remove entries older than 1 second
	filtered := cs.msgTimes[:0]
	for _, t := range cs.msgTimes {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	cs.msgTimes = append(filtered, now)

	return len(cs.msgTimes) <= maxMsgsPerSec
}

// checkOpRateLimit returns true when the connection stays under the per-second
// primitive operation budget.
func (s *Server) checkOpRateLimit(conn *websocket.Conn) bool {
	const maxOpsPerSec = 50
	now := time.Now()
	cutoff := now.Add(-time.Second)

	s.connStatesMu.Lock()
	cs, ok := s.connStates[conn]
	s.connStatesMu.Unlock()
	if !ok {
		return true
	}

	cs.mu.Lock()
	defer cs.mu.Unlock()

	filtered := cs.opTimes[:0]
	for _, t := range cs.opTimes {
		if t.After(cutoff) {
			filtered = append(filtered, t)
		}
	}
	cs.opTimes = append(filtered, now)
	return len(cs.opTimes) <= maxOpsPerSec
}

// pingLoop sends periodic pings and resets the read deadline on pong.
// It runs as a per-connection goroutine (R1).
func (s *Server) pingLoop(conn *websocket.Conn) {
	const pingInterval = 30 * time.Second
	const pongTimeout = 60 * time.Second

	conn.SetPongHandler(func(string) error {
		return conn.SetReadDeadline(time.Now().Add(pongTimeout))
	})
	// Set initial read deadline
	_ = conn.SetReadDeadline(time.Now().Add(pongTimeout))

	ticker := time.NewTicker(pingInterval)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.writeMuMu.Lock()
			mu, exists := s.writeMu[conn]
			s.writeMuMu.Unlock()
			if !exists {
				return
			}
			mu.Lock()
			err := conn.WriteMessage(websocket.PingMessage, nil)
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}
}

// HandleStatic serves static files from the embedded filesystem.
// embed.FS is read-only and path-safe by construction; no traversal guard
// is needed — http.FileServer cleans paths before opening them.
func (s *Server) HandleStatic(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	sub, err := fs.Sub(staticFS, "static")
	if err != nil {
		http.Error(w, "Internal Server Error", http.StatusInternalServerError)
		return
	}
	http.FileServer(http.FS(sub)).ServeHTTP(w, r)
}

// handlePrometheusMetrics emits metrics in Prometheus text format (O2).
func (s *Server) handlePrometheusMetrics(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	g := s.metricsCollector.GetGlobalMetrics()
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, "# HELP syncprim_primitives_total Total registered primitives\n")
	fmt.Fprintf(w, "# TYPE syncprim_primitives_total gauge\n")
	fmt.Fprintf(w, "syncprim_primitives_total %d\n", g.TotalPrimitives)

	fmt.Fprintf(w, "# HELP syncprim_acquires_total Total acquire operations\n")
	fmt.Fprintf(w, "# TYPE syncprim_acquires_total counter\n")
	fmt.Fprintf(w, "syncprim_acquires_total %d\n", g.TotalAcquires)

	fmt.Fprintf(w, "# HELP syncprim_releases_total Total release operations\n")
	fmt.Fprintf(w, "# TYPE syncprim_releases_total counter\n")
	fmt.Fprintf(w, "syncprim_releases_total %d\n", g.TotalReleases)

	fmt.Fprintf(w, "# HELP syncprim_waits_total Total wait operations\n")
	fmt.Fprintf(w, "# TYPE syncprim_waits_total counter\n")
	fmt.Fprintf(w, "syncprim_waits_total %d\n", g.TotalWaits)

	fmt.Fprintf(w, "# HELP syncprim_timeouts_total Total timeout operations\n")
	fmt.Fprintf(w, "# TYPE syncprim_timeouts_total counter\n")
	fmt.Fprintf(w, "syncprim_timeouts_total %d\n", g.TotalTimeouts)

	fmt.Fprintf(w, "# HELP syncprim_contentions_total Total contention events\n")
	fmt.Fprintf(w, "# TYPE syncprim_contentions_total counter\n")
	fmt.Fprintf(w, "syncprim_contentions_total %d\n", g.TotalContentions)

	fmt.Fprintf(w, "# HELP syncprim_uptime_seconds Server uptime in seconds\n")
	fmt.Fprintf(w, "# TYPE syncprim_uptime_seconds gauge\n")
	fmt.Fprintf(w, "syncprim_uptime_seconds %.3f\n", g.Uptime.Seconds())

	fmt.Fprintf(w, "# HELP syncprim_dropped_broadcasts_total Dropped broadcast messages\n")
	fmt.Fprintf(w, "# TYPE syncprim_dropped_broadcasts_total counter\n")
	fmt.Fprintf(w, "syncprim_dropped_broadcasts_total %d\n", s.droppedBroadcasts.Load())
	fmt.Fprintf(w, "# HELP syncprim_rate_limit_hits_total Global message rate-limit rejections\n")
	fmt.Fprintf(w, "# TYPE syncprim_rate_limit_hits_total counter\n")
	fmt.Fprintf(w, "syncprim_rate_limit_hits_total %d\n", s.msgRateLimitHits.Load())
	fmt.Fprintf(w, "# HELP syncprim_op_rate_limit_hits_total Primitive-op rate-limit rejections\n")
	fmt.Fprintf(w, "# TYPE syncprim_op_rate_limit_hits_total counter\n")
	fmt.Fprintf(w, "syncprim_op_rate_limit_hits_total %d\n", s.opRateLimitHits.Load())

	// Per-primitive wait duration histograms.
	allMetrics := s.metricsCollector.GetAllMetrics()
	if len(allMetrics) > 0 {
		fmt.Fprintf(w, "# HELP syncprim_wait_duration_seconds Wait duration histogram\n")
		fmt.Fprintf(w, "# TYPE syncprim_wait_duration_seconds histogram\n")
		for id, snap := range allMetrics {
			for _, bucket := range snap.Histogram {
				var leStr string
				if bucket.Le > 1e308 { // +Inf
					leStr = "+Inf"
				} else {
					leStr = fmt.Sprintf("%g", bucket.Le)
				}
				fmt.Fprintf(w, "syncprim_wait_duration_seconds_bucket{primitive=%q,le=%q} %d\n",
					id, leStr, bucket.Count)
			}
			totalSec := float64(snap.TotalWaitTime.Nanoseconds()) / 1e9
			fmt.Fprintf(w, "syncprim_wait_duration_seconds_sum{primitive=%q} %g\n", id, totalSec)
			fmt.Fprintf(w, "syncprim_wait_duration_seconds_count{primitive=%q} %d\n", id, snap.Waits)
		}
	}
}

// handleHealthz returns server health (O3).
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	m := s.scheduler.GetMetrics()
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]interface{}{
		"status":             "ok",
		"uptime_seconds":     m.Uptime.Seconds(),
		"dropped_broadcasts": s.droppedBroadcasts.Load(),
		"rate_limit_hits":    s.msgRateLimitHits.Load(),
		"op_rate_limit_hits": s.opRateLimitHits.Load(),
	})
}

// handleReadyz returns readiness (O3).
func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", "GET")
		http.Error(w, "Method Not Allowed", http.StatusMethodNotAllowed)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"status": "ready"})
}

// registerClient registers a new WebSocket client and creates its per-connection primitive map.
func (s *Server) registerClient(conn *websocket.Conn) {
	s.clientsMu.Lock()
	s.clients[conn] = true
	s.clientsMu.Unlock()

	s.writeMuMu.Lock()
	s.writeMu[conn] = &sync.Mutex{}
	s.writeMuMu.Unlock()

	s.connStatesMu.Lock()
	s.connStates[conn] = &connState{}
	s.connStatesMu.Unlock()

	s.deltaStatesMu.Lock()
	s.deltaStates[conn] = &deltaState{
		lastPrimJSON: make(map[string]string),
	}
	s.deltaStatesMu.Unlock()

	s.connPrimsMu.Lock()
	s.connPrimsMap[conn] = newConnPrims()
	s.connPrimsMu.Unlock()

	slog.Info("Client connected", "addr", conn.RemoteAddr())
}

// unregisterClient unregisters a WebSocket client and removes its per-connection primitive map.
func (s *Server) unregisterClient(conn *websocket.Conn) {
	s.clientsMu.Lock()
	delete(s.clients, conn)
	s.clientsMu.Unlock()

	s.writeMuMu.Lock()
	delete(s.writeMu, conn)
	s.writeMuMu.Unlock()

	s.connStatesMu.Lock()
	delete(s.connStates, conn)
	s.connStatesMu.Unlock()

	s.deltaStatesMu.Lock()
	delete(s.deltaStates, conn)
	s.deltaStatesMu.Unlock()

	s.connPrimsMu.Lock()
	if cp, ok := s.connPrimsMap[conn]; ok {
		cp.primCtxsMu.Lock()
		for id, entry := range cp.primCtxs {
			entry.cancel()
			delete(cp.primCtxs, id)
		}
		cp.primCtxsMu.Unlock()
	}
	delete(s.connPrimsMap, conn)
	s.connPrimsMu.Unlock()

	s.activeConns.Add(-1)

	conn.Close()
	slog.Info("Client disconnected", "addr", conn.RemoteAddr())
}

// sendInitialState sends the initial state to a client
func (s *Server) sendInitialState(conn *websocket.Conn) {
	prims := s.scheduler.GetPrimitives()
	s.seedDeltaState(conn, prims)

	state := map[string]interface{}{
		"primitives": prims,
		"goroutines": s.scheduler.GetGoroutines(),
		"events":     s.scheduler.GetEvents(100),
		"metrics":    s.scheduler.GetMetrics(),
		"sequence":   s.updateSequence.Load(),
	}

	s.sendToClient(conn, Message{
		Type:    "initialState",
		Payload: jsonMarshal(state),
	})
}

// seedDeltaState records a full snapshot baseline for delta broadcasts.
func (s *Server) seedDeltaState(conn *websocket.Conn, prims map[string]*scheduler.PrimitiveInfo) {
	s.deltaStatesMu.Lock()
	ds, ok := s.deltaStates[conn]
	if !ok {
		ds = &deltaState{lastPrimJSON: make(map[string]string)}
		s.deltaStates[conn] = ds
	}

	seed := make(map[string]string, len(prims))
	for id, prim := range prims {
		seed[id] = primitiveSnapshotKey(prim)
	}
	ds.lastPrimJSON = seed
	s.deltaStatesMu.Unlock()
}

// handleMessage handles an incoming message from a client
func (s *Server) handleMessage(conn *websocket.Conn, msg Message) {
	slog.Info("Received message", "type", msg.Type)

	switch msg.Type {
	case "createRWLock":
		s.handleCreateRWLock(conn, msg)
	case "createSemaphore":
		s.handleCreateSemaphore(conn, msg)
	case "createMutex":
		s.handleCreateMutex(conn, msg)
	case "createCondVar":
		s.handleCreateCondVar(conn, msg)
	case "createBarrier":
		s.handleCreateBarrier(conn, msg)
	case "createWaitGroup":
		s.handleCreateWaitGroup(conn, msg)
	case "createOnce":
		s.handleCreateOnce(conn, msg)
	case "createSingleflight":
		s.handleCreateSingleflight(conn, msg)
	case "deletePrimitive":
		s.handleDeletePrimitive(conn, msg)
	case "getMetrics":
		s.handleGetMetrics(conn, msg)
	// Operation messages from the frontend buttons
	case "primitiveOp":
		s.handlePrimitiveOp(conn, msg)
	case "requestFullRefresh":
		s.handleRequestFullRefresh(conn)
	default:
		s.sendError(conn, fmt.Sprintf("Unknown message type: %s", msg.Type))
	}
}

func (s *Server) handleRequestFullRefresh(conn *websocket.Conn) {
	const minInterval = time.Second

	now := time.Now()
	s.deltaStatesMu.Lock()
	ds, ok := s.deltaStates[conn]
	if !ok {
		ds = &deltaState{lastPrimJSON: make(map[string]string)}
		s.deltaStates[conn] = ds
	}
	if !ds.lastFullRefreshAt.IsZero() && now.Sub(ds.lastFullRefreshAt) < minInterval {
		s.deltaStatesMu.Unlock()
		s.sendError(conn, "full refresh rate limit exceeded: max 1 request/second")
		return
	}
	ds.lastFullRefreshAt = now
	s.deltaStatesMu.Unlock()

	s.fullRefreshRequests.Add(1)
	s.sendState(conn)
}

func (s *Server) sendState(conn *websocket.Conn) {
	prims := s.scheduler.GetPrimitives()
	s.seedDeltaState(conn, prims)

	state := map[string]interface{}{
		"primitives": prims,
		"goroutines": s.scheduler.GetGoroutines(),
		"events":     s.scheduler.GetEvents(100),
		"metrics":    s.scheduler.GetMetrics(),
		"sequence":   s.updateSequence.Load(),
	}
	s.sendToClient(conn, Message{
		Type:    "state",
		Payload: jsonMarshal(state),
	})
}

// connPrimsFor returns the per-connection primitive store for conn, or nil if
// not found (which can happen if the connection has already been unregistered).
func (s *Server) connPrimsFor(conn *websocket.Conn) *connPrims {
	s.connPrimsMu.RLock()
	cp := s.connPrimsMap[conn]
	s.connPrimsMu.RUnlock()
	return cp
}

// handleCreateRWLock creates a new RWLock for the requesting connection.
func (s *Server) handleCreateRWLock(conn *websocket.Conn, msg Message) {
	var payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		s.sendError(conn, "Invalid payload")
		return
	}

	if e := validatePrimID(payload.ID); e != "" {
		s.sendError(conn, e)
		return
	}
	if e := validatePrimName(payload.Name); e != "" {
		s.sendError(conn, e)
		return
	}

	cp := s.connPrimsFor(conn)
	if cp == nil {
		s.sendError(conn, "connection not registered")
		return
	}

	cp.mu.Lock()
	if cp.primExists(payload.ID) {
		cp.mu.Unlock()
		s.sendError(conn, "primitive with ID already exists: "+payload.ID)
		return
	}
	rwlock := primitives.NewRWLock()
	cp.rwlocks[payload.ID] = rwlock
	// Register per-primitive context (Fix 4).
	primCtx, primCancel := context.WithCancel(s.shutdownCtx)
	cp.primCtxsMu.Lock()
	cp.primCtxs[payload.ID] = primEntry{ctx: primCtx, cancel: primCancel}
	cp.primCtxsMu.Unlock()
	cp.mu.Unlock()

	s.scheduler.RegisterPrimitive(payload.ID, scheduler.TypeRWLock, payload.Name, rwlock.GetStats())
	s.metricsCollector.RegisterPrimitive(payload.ID, string(scheduler.TypeRWLock), payload.Name)

	s.sendSuccess(conn, "RWLock created")
}

// handleCreateSemaphore creates a new Semaphore for the requesting connection.
func (s *Server) handleCreateSemaphore(conn *websocket.Conn, msg Message) {
	var payload struct {
		ID       string `json:"id"`
		Name     string `json:"name"`
		Capacity int32  `json:"capacity"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		s.sendError(conn, "Invalid payload")
		return
	}

	if e := validatePrimID(payload.ID); e != "" {
		s.sendError(conn, e)
		return
	}
	if e := validatePrimName(payload.Name); e != "" {
		s.sendError(conn, e)
		return
	}

	if payload.Capacity <= 0 {
		s.sendError(conn, "capacity must be a positive integer")
		return
	}

	cp := s.connPrimsFor(conn)
	if cp == nil {
		s.sendError(conn, "connection not registered")
		return
	}

	cp.mu.Lock()
	if cp.primExists(payload.ID) {
		cp.mu.Unlock()
		s.sendError(conn, "primitive with ID already exists: "+payload.ID)
		return
	}
	semaphore := primitives.NewSemaphore(payload.Capacity)
	cp.semaphores[payload.ID] = semaphore
	primCtx, primCancel := context.WithCancel(s.shutdownCtx)
	cp.primCtxsMu.Lock()
	cp.primCtxs[payload.ID] = primEntry{ctx: primCtx, cancel: primCancel}
	cp.primCtxsMu.Unlock()
	cp.mu.Unlock()

	s.scheduler.RegisterPrimitive(payload.ID, scheduler.TypeSemaphore, payload.Name, semaphore.GetStats())
	s.metricsCollector.RegisterPrimitive(payload.ID, string(scheduler.TypeSemaphore), payload.Name)

	s.sendSuccess(conn, "Semaphore created")
}

// handleCreateMutex creates a new Mutex for the requesting connection.
func (s *Server) handleCreateMutex(conn *websocket.Conn, msg Message) {
	var payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		s.sendError(conn, "Invalid payload")
		return
	}

	if e := validatePrimID(payload.ID); e != "" {
		s.sendError(conn, e)
		return
	}
	if e := validatePrimName(payload.Name); e != "" {
		s.sendError(conn, e)
		return
	}

	cp := s.connPrimsFor(conn)
	if cp == nil {
		s.sendError(conn, "connection not registered")
		return
	}

	cp.mu.Lock()
	if cp.primExists(payload.ID) {
		cp.mu.Unlock()
		s.sendError(conn, "primitive with ID already exists: "+payload.ID)
		return
	}
	mutex := primitives.NewMutex()
	cp.mutexes[payload.ID] = mutex
	primCtx, primCancel := context.WithCancel(s.shutdownCtx)
	cp.primCtxsMu.Lock()
	cp.primCtxs[payload.ID] = primEntry{ctx: primCtx, cancel: primCancel}
	cp.primCtxsMu.Unlock()
	cp.mu.Unlock()

	s.scheduler.RegisterPrimitive(payload.ID, scheduler.TypeMutex, payload.Name, mutex.GetStats())
	s.metricsCollector.RegisterPrimitive(payload.ID, string(scheduler.TypeMutex), payload.Name)

	s.sendSuccess(conn, "Mutex created")
}

// handleCreateCondVar creates a new CondVar for the requesting connection.
func (s *Server) handleCreateCondVar(conn *websocket.Conn, msg Message) {
	var payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		s.sendError(conn, "Invalid payload")
		return
	}

	if e := validatePrimID(payload.ID); e != "" {
		s.sendError(conn, e)
		return
	}
	if e := validatePrimName(payload.Name); e != "" {
		s.sendError(conn, e)
		return
	}

	cp := s.connPrimsFor(conn)
	if cp == nil {
		s.sendError(conn, "connection not registered")
		return
	}

	cp.mu.Lock()
	if cp.primExists(payload.ID) {
		cp.mu.Unlock()
		s.sendError(conn, "primitive with ID already exists: "+payload.ID)
		return
	}
	condvar := primitives.NewCondVar()
	cp.condvars[payload.ID] = condvar
	primCtx, primCancel := context.WithCancel(s.shutdownCtx)
	cp.primCtxsMu.Lock()
	cp.primCtxs[payload.ID] = primEntry{ctx: primCtx, cancel: primCancel}
	cp.primCtxsMu.Unlock()
	cp.mu.Unlock()

	s.scheduler.RegisterPrimitive(payload.ID, scheduler.TypeCondVar, payload.Name, condvar.GetStats())
	s.metricsCollector.RegisterPrimitive(payload.ID, string(scheduler.TypeCondVar), payload.Name)

	s.sendSuccess(conn, "CondVar created")
}

// handleCreateBarrier creates a new Barrier for the requesting connection.
func (s *Server) handleCreateBarrier(conn *websocket.Conn, msg Message) {
	var payload struct {
		ID      string `json:"id"`
		Name    string `json:"name"`
		Parties int32  `json:"parties"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		s.sendError(conn, "Invalid payload")
		return
	}

	if e := validatePrimID(payload.ID); e != "" {
		s.sendError(conn, e)
		return
	}
	if e := validatePrimName(payload.Name); e != "" {
		s.sendError(conn, e)
		return
	}

	if payload.Parties <= 0 {
		s.sendError(conn, "parties must be a positive integer")
		return
	}

	cp := s.connPrimsFor(conn)
	if cp == nil {
		s.sendError(conn, "connection not registered")
		return
	}

	cp.mu.Lock()
	if cp.primExists(payload.ID) {
		cp.mu.Unlock()
		s.sendError(conn, "primitive with ID already exists: "+payload.ID)
		return
	}
	barrier := primitives.NewBarrier(payload.Parties)
	cp.barriers[payload.ID] = barrier
	primCtx, primCancel := context.WithCancel(s.shutdownCtx)
	cp.primCtxsMu.Lock()
	cp.primCtxs[payload.ID] = primEntry{ctx: primCtx, cancel: primCancel}
	cp.primCtxsMu.Unlock()
	cp.mu.Unlock()

	s.scheduler.RegisterPrimitive(payload.ID, scheduler.TypeBarrier, payload.Name, barrier.GetStats())
	s.metricsCollector.RegisterPrimitive(payload.ID, string(scheduler.TypeBarrier), payload.Name)

	s.sendSuccess(conn, "Barrier created")
}

// handleCreateWaitGroup creates a new WaitGroup for the requesting connection.
func (s *Server) handleCreateWaitGroup(conn *websocket.Conn, msg Message) {
	var payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		s.sendError(conn, "Invalid payload")
		return
	}
	if e := validatePrimID(payload.ID); e != "" {
		s.sendError(conn, e)
		return
	}
	if e := validatePrimName(payload.Name); e != "" {
		s.sendError(conn, e)
		return
	}
	cp := s.connPrimsFor(conn)
	if cp == nil {
		s.sendError(conn, "connection not registered")
		return
	}
	cp.mu.Lock()
	if cp.primExists(payload.ID) {
		cp.mu.Unlock()
		s.sendError(conn, "primitive with ID already exists: "+payload.ID)
		return
	}
	wg := primitives.NewWaitGroup()
	cp.waitgroups[payload.ID] = wg
	primCtx, primCancel := context.WithCancel(s.shutdownCtx)
	cp.primCtxsMu.Lock()
	cp.primCtxs[payload.ID] = primEntry{ctx: primCtx, cancel: primCancel}
	cp.primCtxsMu.Unlock()
	cp.mu.Unlock()
	s.scheduler.RegisterPrimitive(payload.ID, scheduler.TypeWaitGroup, payload.Name, wg.GetStats())
	s.metricsCollector.RegisterPrimitive(payload.ID, string(scheduler.TypeWaitGroup), payload.Name)
	s.sendSuccess(conn, "WaitGroup created")
}

// handleCreateOnce creates a new Once for the requesting connection.
func (s *Server) handleCreateOnce(conn *websocket.Conn, msg Message) {
	var payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		s.sendError(conn, "Invalid payload")
		return
	}
	if e := validatePrimID(payload.ID); e != "" {
		s.sendError(conn, e)
		return
	}
	if e := validatePrimName(payload.Name); e != "" {
		s.sendError(conn, e)
		return
	}
	cp := s.connPrimsFor(conn)
	if cp == nil {
		s.sendError(conn, "connection not registered")
		return
	}
	cp.mu.Lock()
	if cp.primExists(payload.ID) {
		cp.mu.Unlock()
		s.sendError(conn, "primitive with ID already exists: "+payload.ID)
		return
	}
	o := primitives.NewOnce()
	cp.onces[payload.ID] = o
	primCtx, primCancel := context.WithCancel(s.shutdownCtx)
	cp.primCtxsMu.Lock()
	cp.primCtxs[payload.ID] = primEntry{ctx: primCtx, cancel: primCancel}
	cp.primCtxsMu.Unlock()
	cp.mu.Unlock()
	s.scheduler.RegisterPrimitive(payload.ID, scheduler.TypeOnce, payload.Name, o.GetStats())
	s.metricsCollector.RegisterPrimitive(payload.ID, string(scheduler.TypeOnce), payload.Name)
	s.sendSuccess(conn, "Once created")
}

// handleCreateSingleflight creates a new Singleflight Group for the requesting connection.
func (s *Server) handleCreateSingleflight(conn *websocket.Conn, msg Message) {
	var payload struct {
		ID   string `json:"id"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		s.sendError(conn, "Invalid payload")
		return
	}
	if e := validatePrimID(payload.ID); e != "" {
		s.sendError(conn, e)
		return
	}
	if e := validatePrimName(payload.Name); e != "" {
		s.sendError(conn, e)
		return
	}
	cp := s.connPrimsFor(conn)
	if cp == nil {
		s.sendError(conn, "connection not registered")
		return
	}
	cp.mu.Lock()
	if cp.primExists(payload.ID) {
		cp.mu.Unlock()
		s.sendError(conn, "primitive with ID already exists: "+payload.ID)
		return
	}
	g := primitives.NewGroup()
	cp.singleflights[payload.ID] = g
	primCtx, primCancel := context.WithCancel(s.shutdownCtx)
	cp.primCtxsMu.Lock()
	cp.primCtxs[payload.ID] = primEntry{ctx: primCtx, cancel: primCancel}
	cp.primCtxsMu.Unlock()
	cp.mu.Unlock()
	s.scheduler.RegisterPrimitive(payload.ID, scheduler.TypeSingleflight, payload.Name, g.GetStats())
	s.metricsCollector.RegisterPrimitive(payload.ID, string(scheduler.TypeSingleflight), payload.Name)
	s.sendSuccess(conn, "Singleflight created")
}

// handleDeletePrimitive deletes a primitive owned by the requesting connection.
func (s *Server) handleDeletePrimitive(conn *websocket.Conn, msg Message) {
	var payload struct {
		ID string `json:"id"`
	}

	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		s.sendError(conn, "Invalid payload")
		return
	}

	cp := s.connPrimsFor(conn)
	if cp == nil {
		s.sendError(conn, "connection not registered")
		return
	}

	cp.mu.Lock()
	if !cp.primExists(payload.ID) {
		cp.mu.Unlock()
		s.sendError(conn, "Primitive not found: "+payload.ID)
		return
	}
	delete(cp.rwlocks, payload.ID)
	delete(cp.semaphores, payload.ID)
	delete(cp.mutexes, payload.ID)
	delete(cp.condvars, payload.ID)
	delete(cp.barriers, payload.ID)
	delete(cp.waitgroups, payload.ID)
	delete(cp.onces, payload.ID)
	delete(cp.singleflights, payload.ID)
	cp.mu.Unlock()

	// Cancel any blocked operations on this primitive (Fix 4).
	cp.primCtxsMu.Lock()
	if entry, ok := cp.primCtxs[payload.ID]; ok {
		entry.cancel()
		delete(cp.primCtxs, payload.ID)
	}
	cp.primCtxsMu.Unlock()

	s.scheduler.UnregisterPrimitive(payload.ID)
	s.metricsCollector.UnregisterPrimitive(payload.ID)

	s.sendSuccess(conn, "Primitive deleted")
}

// handleGetMetrics sends metrics to the client
func (s *Server) handleGetMetrics(conn *websocket.Conn, msg Message) {
	globalMetrics := s.metricsCollector.GetGlobalMetrics()
	primitiveMetrics := s.metricsCollector.GetAllMetrics()

	response := map[string]interface{}{
		"global":     globalMetrics,
		"primitives": primitiveMetrics,
	}

	s.sendToClient(conn, Message{
		Type:    "metrics",
		Payload: jsonMarshal(response),
	})
}

// maxIDLen and maxNameLen are upper bounds for primitive identifiers and names.
// Enforced on every create handler to prevent memory-exhaustion via oversized strings.
const (
	maxIDLen      = 256
	maxNameLen    = 256
	holdMsMax     = 3_600_000
	holdMsDefault = 100
	operationTimeout = time.Hour
)

// validatePrimID returns an error string when id is empty or exceeds maxIDLen.
// Returns "" when the id is acceptable.
func validatePrimID(id string) string {
	if id == "" {
		return "id must not be empty"
	}
	if len(id) > maxIDLen {
		return fmt.Sprintf("id must not exceed %d characters", maxIDLen)
	}
	return ""
}

// validatePrimName returns an error string when name exceeds maxNameLen.
// Returns "" when the name is acceptable.
func validatePrimName(name string) string {
	if len(name) > maxNameLen {
		return fmt.Sprintf("name must not exceed %d characters", maxNameLen)
	}
	return ""
}

// clampHoldMs clamps an integer to [1, holdMsMax], defaulting to holdMsDefault
// when zero or negative. It returns the clamped duration and optional warning.
func clampHoldMs(ms int) (time.Duration, string) {
	requested := ms
	if ms <= 0 {
		ms = holdMsDefault
	}
	if ms > holdMsMax {
		ms = holdMsMax
	}
	if requested > holdMsMax {
		return time.Duration(ms) * time.Millisecond, fmt.Sprintf("holdMs clamped from %d to %d", requested, ms)
	}
	return time.Duration(ms) * time.Millisecond, ""
}

// handlePrimitiveOp handles operation requests from the frontend buttons.
// The operation is run in a goroutine so it does not block the WebSocket loop.
// Payload: { "id": "<primitive-id>", "op": "<operation>", "holdMs": <int> }
func (s *Server) handlePrimitiveOp(conn *websocket.Conn, msg Message) {
	var payload struct {
		ID     string `json:"id"`
		Op     string `json:"op"`
		HoldMs int    `json:"holdMs"`
	}
	if err := json.Unmarshal(msg.Payload, &payload); err != nil {
		s.sendError(conn, "Invalid payload for primitiveOp")
		return
	}

	holdDuration, holdWarning := clampHoldMs(payload.HoldMs)

	go func() {
		defer func() {
			if r := recover(); r != nil {
				slog.Error("panic in primitiveOp goroutine", "recover", r, "id", payload.ID, "op", payload.Op)
				s.sendError(conn, fmt.Sprintf("internal error: %v", r))
			}
		}()

		// Register this op goroutine with the scheduler so the goroutines
		// panel and BlockedCount reflect live state.
		goroutineID := s.goroutineCounter.Add(1)
		goroutineName := fmt.Sprintf("op:%s:%s", payload.Op, payload.ID)
		s.scheduler.RegisterGoroutine(goroutineID, goroutineName)
		defer s.scheduler.UnregisterGoroutine(goroutineID)

		cp := s.connPrimsFor(conn)
		if cp == nil {
			s.sendError(conn, "connection not registered")
			return
		}

		cp.mu.RLock()
		rwlock := cp.rwlocks[payload.ID]
		semaphore := cp.semaphores[payload.ID]
		mutex := cp.mutexes[payload.ID]
		condvar := cp.condvars[payload.ID]
		barrier := cp.barriers[payload.ID]
		waitgroup := cp.waitgroups[payload.ID]
		once := cp.onces[payload.ID]
		sflight := cp.singleflights[payload.ID]
		cp.mu.RUnlock()

		// Retrieve the per-primitive context for blocking ops (Fix 4).
		// When the primitive is deleted, its cancel func is called which
		// cancels this context, unblocking any goroutine waiting on it.
		cp.primCtxsMu.Lock()
		var baseOpCtx context.Context
		if entry, ok := cp.primCtxs[payload.ID]; ok {
			baseOpCtx = entry.ctx
		} else {
			baseOpCtx = s.shutdownCtx
		}
		cp.primCtxsMu.Unlock()
		opCtxForBlocking, opCancel := context.WithTimeout(baseOpCtx, operationTimeout)
		defer opCancel()

		sendBlockingErr := func(err error) {
			if errors.Is(err, context.DeadlineExceeded) {
				s.sendError(conn, "operation timed out after 1 hour")
				return
			}
			if baseOpCtx.Err() != nil {
				s.sendError(conn, "primitive deleted while operation was in progress")
				return
			}
			s.sendError(conn, "operation cancelled")
		}

		start := time.Now()

		switch payload.Op {
		case "rlock":
			if rwlock == nil {
				s.sendError(conn, "RWLock not found: "+payload.ID)
				return
			}
			s.scheduler.BlockGoroutine(goroutineID, payload.ID)
			blockStart := time.Now()
			if err := rwlock.RLockContext(opCtxForBlocking); err != nil {
				s.scheduler.UnblockGoroutine(goroutineID, time.Since(blockStart))
				sendBlockingErr(err)
				return
			}
			s.scheduler.UnblockGoroutine(goroutineID, time.Since(blockStart))
			s.metricsCollector.RecordAcquire(payload.ID, rwlock.GetStats().CurrentReaders)
			go func() {
				select {
				case <-baseOpCtx.Done():
				case <-time.After(holdDuration):
				}
				rwlock.RUnlock()
				s.metricsCollector.RecordRelease(payload.ID, rwlock.GetStats().CurrentReaders)
			}()

		case "lock":
			if rwlock != nil {
				s.scheduler.BlockGoroutine(goroutineID, payload.ID)
				blockStart := time.Now()
				if err := rwlock.LockContext(opCtxForBlocking); err != nil {
					s.scheduler.UnblockGoroutine(goroutineID, time.Since(blockStart))
					sendBlockingErr(err)
					return
				}
				s.scheduler.UnblockGoroutine(goroutineID, time.Since(blockStart))
				s.metricsCollector.RecordAcquire(payload.ID, 1)
				s.metricsCollector.RecordWait(payload.ID, time.Since(start), int32(rwlock.GetStats().WritersWaiting))
				go func() {
					select {
					case <-baseOpCtx.Done():
					case <-time.After(holdDuration):
					}
					rwlock.Unlock()
					s.metricsCollector.RecordRelease(payload.ID, 0)
				}()
			} else if mutex != nil {
				s.scheduler.BlockGoroutine(goroutineID, payload.ID)
				blockStart := time.Now()
				if err := mutex.LockContext(opCtxForBlocking); err != nil {
					s.scheduler.UnblockGoroutine(goroutineID, time.Since(blockStart))
					sendBlockingErr(err)
					return
				}
				s.scheduler.UnblockGoroutine(goroutineID, time.Since(blockStart))
				s.metricsCollector.RecordAcquire(payload.ID, 1)
				s.metricsCollector.RecordWait(payload.ID, time.Since(start), int32(mutex.GetStats().WaitersQueued))
				go func() {
					select {
					case <-baseOpCtx.Done():
					case <-time.After(holdDuration):
					}
					mutex.Unlock()
					s.metricsCollector.RecordRelease(payload.ID, 0)
				}()
			} else {
				s.sendError(conn, "Lock primitive not found: "+payload.ID)
				return
			}

		case "unlock":
			if mutex == nil {
				s.sendError(conn, "Mutex not found: "+payload.ID)
				return
			}
			if !mutex.IsLocked() {
				s.sendError(conn, "Mutex is not locked: "+payload.ID)
				return
			}
			mutex.Unlock()
			s.metricsCollector.RecordRelease(payload.ID, 0)

		case "acquire":
			if semaphore == nil {
				s.sendError(conn, "Semaphore not found: "+payload.ID)
				return
			}
			s.scheduler.BlockGoroutine(goroutineID, payload.ID)
			blockStart := time.Now()
			if err := semaphore.AcquireContext(opCtxForBlocking); err != nil {
				s.scheduler.UnblockGoroutine(goroutineID, time.Since(blockStart))
				sendBlockingErr(err)
				return
			}
			s.scheduler.UnblockGoroutine(goroutineID, time.Since(blockStart))
			s.metricsCollector.RecordAcquire(payload.ID, semaphore.GetStats().CurrentCount)
			s.metricsCollector.RecordWait(payload.ID, time.Since(start), int32(semaphore.GetStats().WaitersQueued))

		case "release":
			if semaphore == nil {
				s.sendError(conn, "Semaphore not found: "+payload.ID)
				return
			}
			if err := semaphore.Release(); err != nil {
				s.sendError(conn, "Semaphore release error: "+err.Error())
				return
			}
			s.metricsCollector.RecordRelease(payload.ID, semaphore.GetStats().CurrentCount)

		case "signal":
			if condvar == nil {
				s.sendError(conn, "CondVar not found: "+payload.ID)
				return
			}
			condvar.Signal()

		case "broadcast":
			if condvar == nil {
				s.sendError(conn, "CondVar not found: "+payload.ID)
				return
			}
			condvar.Broadcast()

		case "wait":
			if barrier == nil {
				s.sendError(conn, "Barrier not found: "+payload.ID)
				return
			}
			// Derive a 500ms timeout from opCtxForBlocking: the op times out if
			// the barrier never trips, and is cancelled immediately on shutdown/delete.
			s.scheduler.BlockGoroutine(goroutineID, payload.ID)
			blockStart := time.Now()
			timeoutCtx, timeoutCancel := context.WithTimeout(opCtxForBlocking, 500*time.Millisecond)
			_, _ = barrier.WaitContext(timeoutCtx)
			timeoutCancel()
			s.scheduler.UnblockGoroutine(goroutineID, time.Since(blockStart))

		case "reset":
			if barrier == nil {
				s.sendError(conn, "Barrier not found: "+payload.ID)
				return
			}
			barrier.Reset()

		case "add":
			if waitgroup == nil {
				s.sendError(conn, "WaitGroup not found: "+payload.ID)
				return
			}
			delta := payload.HoldMs // reuse HoldMs as the delta parameter
			if delta <= 0 {
				delta = 1
			}
			waitgroup.Add(delta)

		case "done":
			if waitgroup == nil {
				s.sendError(conn, "WaitGroup not found: "+payload.ID)
				return
			}
			waitgroup.Done()

		case "wg-wait":
			if waitgroup == nil {
				s.sendError(conn, "WaitGroup not found: "+payload.ID)
				return
			}
			s.scheduler.BlockGoroutine(goroutineID, payload.ID)
			blockStart := time.Now()
			_ = waitgroup.WaitContext(opCtxForBlocking)
			s.scheduler.UnblockGoroutine(goroutineID, time.Since(blockStart))

		case "do":
			if once != nil {
				once.Do(func() {})
			} else if sflight != nil {
				_, _ = sflight.Do(payload.ID, func() (interface{}, error) {
					time.Sleep(holdDuration)
					return nil, nil
				})
			} else {
				s.sendError(conn, "Once or Singleflight not found: "+payload.ID)
				return
			}

		case "forget":
			if sflight == nil {
				s.sendError(conn, "Singleflight not found: "+payload.ID)
				return
			}
			sflight.Forget(payload.ID)

		default:
			s.sendError(conn, "Unknown operation: "+payload.Op)
			return
		}

		if holdWarning != "" {
			s.sendToClient(conn, Message{
				Type: "success",
				Payload: jsonMarshal(map[string]string{
					"message": fmt.Sprintf("Operation %s on %s completed", payload.Op, payload.ID),
					"warning": holdWarning,
				}),
			})
			return
		}
		s.sendSuccess(conn, fmt.Sprintf("Operation %s on %s completed", payload.Op, payload.ID))
	}()
}

// sendSuccess sends a success message to a client
func (s *Server) sendSuccess(conn *websocket.Conn, message string) {
	s.sendToClient(conn, Message{
		Type:    "success",
		Payload: jsonMarshal(map[string]string{"message": message}),
	})
}

// sendError sends an error message to a client
func (s *Server) sendError(conn *websocket.Conn, message string) {
	s.sendToClient(conn, Message{
		Type:    "error",
		Payload: jsonMarshal(map[string]string{"message": message}),
	})
}

// sendToClient sends a message to a specific client
func (s *Server) sendToClient(conn *websocket.Conn, msg Message) {
	s.writeMuMu.Lock()
	mu, exists := s.writeMu[conn]
	s.writeMuMu.Unlock()

	if !exists {
		return
	}

	mu.Lock()
	defer mu.Unlock()

	if err := conn.WriteJSON(msg); err != nil {
		slog.Error("Error sending message to client", "err", err)
	}
}

// broadcastMessages broadcasts messages to all clients until stop is closed.
func (s *Server) broadcastMessages() {
	for {
		select {
		case <-s.stop:
			return
		case msg, ok := <-s.broadcast:
			if !ok {
				return
			}
			s.clientsMu.RLock()
			clients := make([]*websocket.Conn, 0, len(s.clients))
			for conn := range s.clients {
				clients = append(clients, conn)
			}
			s.clientsMu.RUnlock()

			if update, ok := msg.(*scheduler.SchedulerUpdate); ok {
				sequence := s.updateSequence.Add(1)
				for _, conn := range clients {
					payload := s.buildDeltaUpdatePayload(conn, update, sequence)
					s.sendToClient(conn, Message{
						Type:    "update",
						Payload: jsonMarshal(payload),
					})
				}
				continue
			}

			for _, conn := range clients {
				s.sendToClient(conn, Message{
					Type:    "update",
					Payload: jsonMarshal(msg),
				})
			}
		}
	}
}

type deltaUpdatePayload struct {
	Primitives map[string]*scheduler.PrimitiveInfo `json:"Primitives"`
	Deleted    []string                            `json:"Deleted,omitempty"`
	Goroutines map[uint64]*scheduler.GoroutineInfo `json:"Goroutines"`
	Events     []scheduler.Event                   `json:"Events"`
	Metrics    scheduler.SchedulerMetrics          `json:"Metrics"`
	Sequence   uint64                              `json:"Sequence"`
}

func (s *Server) buildDeltaUpdatePayload(conn *websocket.Conn, update *scheduler.SchedulerUpdate, sequence uint64) deltaUpdatePayload {
	changed := make(map[string]*scheduler.PrimitiveInfo)
	deleted := make([]string, 0)

	s.deltaStatesMu.Lock()
	ds, ok := s.deltaStates[conn]
	if !ok {
		ds = &deltaState{lastPrimJSON: make(map[string]string)}
		s.deltaStates[conn] = ds
	}

	for id, prim := range update.Primitives {
		snapshot := primitiveSnapshotKey(prim)
		if prev, exists := ds.lastPrimJSON[id]; !exists || prev != snapshot {
			changed[id] = prim
			ds.lastPrimJSON[id] = snapshot
		}
	}

	for id := range ds.lastPrimJSON {
		if _, exists := update.Primitives[id]; exists {
			continue
		}
		deleted = append(deleted, id)
		delete(ds.lastPrimJSON, id)
	}
	s.deltaStatesMu.Unlock()

	return deltaUpdatePayload{
		Primitives: changed,
		Deleted:    deleted,
		Goroutines: update.Goroutines,
		Events:     update.Events,
		Metrics:    update.Metrics,
		Sequence:   sequence,
	}
}

// primitiveSnapshotKey normalizes a primitive into a stable string key used
// for delta comparisons. It excludes volatile stats fields such as Age so
// idle primitives are not treated as changed on every tick.
func primitiveSnapshotKey(prim *scheduler.PrimitiveInfo) string {
	data, err := json.Marshal(prim)
	if err != nil {
		return ""
	}
	var asMap map[string]interface{}
	if err := json.Unmarshal(data, &asMap); err != nil {
		return string(data)
	}
	if statsAny, ok := asMap["Stats"]; ok {
		if statsMap, ok := statsAny.(map[string]interface{}); ok {
			delete(statsMap, "Age")
		}
	}
	normalized, err := json.Marshal(asMap)
	if err != nil {
		return string(data)
	}
	return string(normalized)
}

// forwardSchedulerUpdates forwards scheduler updates to broadcast channel
// until the scheduler is stopped.
func (s *Server) forwardSchedulerUpdates() {
	updateChan := s.scheduler.GetUpdateChannel()
	done := s.scheduler.Done()

	for {
		select {
		case <-done:
			return
		case update := <-updateChan:
			select {
			case s.broadcast <- update:
			case <-done:
				return
			default:
				// Broadcast channel full; increment drop counter and log.
				dropped := s.droppedBroadcasts.Add(1)
				slog.Warn("Broadcast channel full, dropping update", "totalDropped", dropped)
			}
		}
	}
}

// updatePrimitiveStats periodically pushes fresh stats from all live
// primitives (across all connections) into the scheduler so the dashboard
// shows current numbers.
func (s *Server) updatePrimitiveStats() {
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
		}

		s.connPrimsMu.RLock()
		for _, cp := range s.connPrimsMap {
			cp.mu.RLock()
			for id, rw := range cp.rwlocks {
				s.scheduler.UpdatePrimitiveStats(id, rw.GetStats())
			}
			for id, sem := range cp.semaphores {
				s.scheduler.UpdatePrimitiveStats(id, sem.GetStats())
			}
			for id, m := range cp.mutexes {
				s.scheduler.UpdatePrimitiveStats(id, m.GetStats())
			}
			for id, cv := range cp.condvars {
				s.scheduler.UpdatePrimitiveStats(id, cv.GetStats())
			}
			for id, b := range cp.barriers {
				s.scheduler.UpdatePrimitiveStats(id, b.GetStats())
			}
			for id, wg := range cp.waitgroups {
				s.scheduler.UpdatePrimitiveStats(id, wg.GetStats())
			}
			for id, o := range cp.onces {
				s.scheduler.UpdatePrimitiveStats(id, o.GetStats())
			}
			for id, g := range cp.singleflights {
				s.scheduler.UpdatePrimitiveStats(id, g.GetStats())
			}
			cp.mu.RUnlock()
		}
		s.connPrimsMu.RUnlock()
	}
}

// HandleHealthz is the exported health endpoint (wraps handleHealthz for tests/mux).
func (s *Server) HandleHealthz(w http.ResponseWriter, r *http.Request) {
	s.handleHealthz(w, r)
}

// HandleReadyz is the exported readiness endpoint.
func (s *Server) HandleReadyz(w http.ResponseWriter, r *http.Request) {
	s.handleReadyz(w, r)
}

// HandlePrometheusMetrics is the exported Prometheus metrics endpoint.
func (s *Server) HandlePrometheusMetrics(w http.ResponseWriter, r *http.Request) {
	s.handlePrometheusMetrics(w, r)
}

// GetSchedulerMetrics exposes scheduler metrics for tests.
func (s *Server) GetSchedulerMetrics() scheduler.SchedulerMetrics {
	return s.scheduler.GetMetrics()
}

// GetSchedulerPrimitives exposes the scheduler primitive map for tests.
func (s *Server) GetSchedulerPrimitives() map[string]*scheduler.PrimitiveInfo {
	return s.scheduler.GetPrimitives()
}

// primitiveSnapshot is the on-disk representation of a single primitive.
// Only the fields needed to recreate the primitive are stored.
type primitiveSnapshot struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Name     string `json:"name"`
	Capacity int32  `json:"capacity,omitempty"` // Semaphore
	Parties  int32  `json:"parties,omitempty"`  // Barrier
}

const snapshotVersion = 1

// snapshotFile is the top-level structure written to disk.
type snapshotFile struct {
	Version    int                 `json:"version"`
	Primitives []primitiveSnapshot `json:"primitives"`
}

// legacyPrimitiveSnapshot is the historical unversioned on-disk format where
// primitive IDs were map keys instead of embedded fields.
type legacyPrimitiveSnapshot struct {
	Type     string `json:"type"`
	Name     string `json:"name"`
	Capacity int32  `json:"capacity,omitempty"`
	Parties  int32  `json:"parties,omitempty"`
}

// saveSnapshot writes the currently registered primitives (from all
// connections) to s.snapshotPath as a JSON file.  It is called by Shutdown
// so state survives restarts.  Errors are logged but not returned to the
// caller — a snapshot failure must not prevent graceful shutdown.
func (s *Server) saveSnapshot() {
	if s.snapshotPath == "" {
		return
	}

	// Fetch names from the scheduler (single source of truth for names).
	schedPrims := s.scheduler.GetPrimitives()
	nameFor := func(id string) string {
		if info, ok := schedPrims[id]; ok {
			return info.Name
		}
		return id
	}

	var snap snapshotFile
	snap.Version = snapshotVersion

	s.connPrimsMu.RLock()
	for _, cp := range s.connPrimsMap {
		cp.mu.RLock()
		for id := range cp.rwlocks {
			snap.Primitives = append(snap.Primitives, primitiveSnapshot{
				ID:   id,
				Type: string(scheduler.TypeRWLock),
				Name: nameFor(id),
			})
		}
		for id, sem := range cp.semaphores {
			st := sem.GetStats()
			snap.Primitives = append(snap.Primitives, primitiveSnapshot{
				ID:       id,
				Type:     string(scheduler.TypeSemaphore),
				Name:     nameFor(id),
				Capacity: st.Capacity,
			})
		}
		for id := range cp.mutexes {
			snap.Primitives = append(snap.Primitives, primitiveSnapshot{
				ID:   id,
				Type: string(scheduler.TypeMutex),
				Name: nameFor(id),
			})
		}
		for id := range cp.condvars {
			snap.Primitives = append(snap.Primitives, primitiveSnapshot{
				ID:   id,
				Type: string(scheduler.TypeCondVar),
				Name: nameFor(id),
			})
		}
		for id, b := range cp.barriers {
			st := b.GetStats()
			snap.Primitives = append(snap.Primitives, primitiveSnapshot{
				ID:      id,
				Type:    string(scheduler.TypeBarrier),
				Name:    nameFor(id),
				Parties: st.Parties,
			})
		}
		for id := range cp.waitgroups {
			snap.Primitives = append(snap.Primitives, primitiveSnapshot{
				ID:   id,
				Type: string(scheduler.TypeWaitGroup),
				Name: nameFor(id),
			})
		}
		for id := range cp.onces {
			snap.Primitives = append(snap.Primitives, primitiveSnapshot{
				ID:   id,
				Type: string(scheduler.TypeOnce),
				Name: nameFor(id),
			})
		}
		for id := range cp.singleflights {
			snap.Primitives = append(snap.Primitives, primitiveSnapshot{
				ID:   id,
				Type: string(scheduler.TypeSingleflight),
				Name: nameFor(id),
			})
		}
		cp.mu.RUnlock()
	}
	s.connPrimsMu.RUnlock()

	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		slog.Error("snapshot: marshal failed", "err", err)
		return
	}
	if err := os.WriteFile(s.snapshotPath, data, 0o600); err != nil {
		slog.Error("snapshot: write failed", "path", s.snapshotPath, "err", err)
		return
	}
	slog.Info("snapshot saved", "path", s.snapshotPath, "primitives", len(snap.Primitives))
}

// loadSnapshot reads the snapshot file (if any) and recreates the primitives
// into a synthetic "snapshot" connPrims that is registered under a nil conn
// sentinel key.  Since there is no real WebSocket connection at startup, we
// use a dedicated sentinel connPrims entry.
//
// Missing or empty snapshot files are silently ignored.  Corrupt files log a
// warning and are skipped.
func (s *Server) loadSnapshot() {
	if s.snapshotPath == "" {
		return
	}

	data, err := os.ReadFile(s.snapshotPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return // no snapshot yet — normal on first run
		}
		slog.Warn("snapshot: read failed", "path", s.snapshotPath, "err", err)
		return
	}
	if len(data) == 0 {
		return
	}

	var snap snapshotFile
	if err := json.Unmarshal(data, &snap); err == nil && snap.Version > 0 {
		if snap.Version != snapshotVersion {
			slog.Warn(
				"snapshot: unsupported version; skipping load",
				"path",
				s.snapshotPath,
				"file_version",
				snap.Version,
				"supported_version",
				snapshotVersion,
			)
			return
		}
		s.restoreSnapshotPrimitives(snap.Primitives)
		return
	}

	// Backward compatibility: accept legacy unversioned map snapshots.
	var legacy map[string]legacyPrimitiveSnapshot
	if err := json.Unmarshal(data, &legacy); err != nil {
		slog.Warn("snapshot: parse failed", "path", s.snapshotPath, "err", err)
		return
	}
	legacyPrims := make([]primitiveSnapshot, 0, len(legacy))
	for id, p := range legacy {
		legacyPrims = append(legacyPrims, primitiveSnapshot{
			ID:       id,
			Type:     p.Type,
			Name:     p.Name,
			Capacity: p.Capacity,
			Parties:  p.Parties,
		})
	}
	if len(legacyPrims) > 0 {
		slog.Warn(
			"snapshot: loaded legacy unversioned format; will be upgraded on next save",
			"path",
			s.snapshotPath,
			"primitives",
			len(legacyPrims),
		)
	}
	s.restoreSnapshotPrimitives(legacyPrims)
}

func (s *Server) restoreSnapshotPrimitives(prims []primitiveSnapshot) {
	// Use a sentinel connPrims entry keyed under nil to hold restored primitives.
	cp := newConnPrims()
	s.connPrimsMu.Lock()
	s.connPrimsMap[nil] = cp
	s.connPrimsMu.Unlock()

	restored := 0
	for _, p := range prims {
		if p.ID == "" {
			continue
		}
		switch p.Type {
		case string(scheduler.TypeRWLock):
			rw := primitives.NewRWLock()
			cp.mu.Lock()
			cp.rwlocks[p.ID] = rw
			primCtx, primCancel := context.WithCancel(s.shutdownCtx)
			cp.primCtxsMu.Lock()
			cp.primCtxs[p.ID] = primEntry{ctx: primCtx, cancel: primCancel}
			cp.primCtxsMu.Unlock()
			cp.mu.Unlock()
			s.scheduler.RegisterPrimitive(p.ID, scheduler.TypeRWLock, p.Name, rw.GetStats())
			s.metricsCollector.RegisterPrimitive(p.ID, p.Type, p.Name)
			restored++
		case string(scheduler.TypeSemaphore):
			cap := p.Capacity
			if cap <= 0 {
				cap = 1
			}
			sem := primitives.NewSemaphore(cap)
			cp.mu.Lock()
			cp.semaphores[p.ID] = sem
			primCtx, primCancel := context.WithCancel(s.shutdownCtx)
			cp.primCtxsMu.Lock()
			cp.primCtxs[p.ID] = primEntry{ctx: primCtx, cancel: primCancel}
			cp.primCtxsMu.Unlock()
			cp.mu.Unlock()
			s.scheduler.RegisterPrimitive(p.ID, scheduler.TypeSemaphore, p.Name, sem.GetStats())
			s.metricsCollector.RegisterPrimitive(p.ID, p.Type, p.Name)
			restored++
		case string(scheduler.TypeMutex):
			m := primitives.NewMutex()
			cp.mu.Lock()
			cp.mutexes[p.ID] = m
			primCtx, primCancel := context.WithCancel(s.shutdownCtx)
			cp.primCtxsMu.Lock()
			cp.primCtxs[p.ID] = primEntry{ctx: primCtx, cancel: primCancel}
			cp.primCtxsMu.Unlock()
			cp.mu.Unlock()
			s.scheduler.RegisterPrimitive(p.ID, scheduler.TypeMutex, p.Name, m.GetStats())
			s.metricsCollector.RegisterPrimitive(p.ID, p.Type, p.Name)
			restored++
		case string(scheduler.TypeCondVar):
			cv := primitives.NewCondVar()
			cp.mu.Lock()
			cp.condvars[p.ID] = cv
			primCtx, primCancel := context.WithCancel(s.shutdownCtx)
			cp.primCtxsMu.Lock()
			cp.primCtxs[p.ID] = primEntry{ctx: primCtx, cancel: primCancel}
			cp.primCtxsMu.Unlock()
			cp.mu.Unlock()
			s.scheduler.RegisterPrimitive(p.ID, scheduler.TypeCondVar, p.Name, cv.GetStats())
			s.metricsCollector.RegisterPrimitive(p.ID, p.Type, p.Name)
			restored++
		case string(scheduler.TypeBarrier):
			parties := p.Parties
			if parties <= 0 {
				parties = 1
			}
			b := primitives.NewBarrier(parties)
			cp.mu.Lock()
			cp.barriers[p.ID] = b
			primCtx, primCancel := context.WithCancel(s.shutdownCtx)
			cp.primCtxsMu.Lock()
			cp.primCtxs[p.ID] = primEntry{ctx: primCtx, cancel: primCancel}
			cp.primCtxsMu.Unlock()
			cp.mu.Unlock()
			s.scheduler.RegisterPrimitive(p.ID, scheduler.TypeBarrier, p.Name, b.GetStats())
			s.metricsCollector.RegisterPrimitive(p.ID, p.Type, p.Name)
			restored++
		case string(scheduler.TypeWaitGroup):
			wg := primitives.NewWaitGroup()
			cp.mu.Lock()
			cp.waitgroups[p.ID] = wg
			primCtx, primCancel := context.WithCancel(s.shutdownCtx)
			cp.primCtxsMu.Lock()
			cp.primCtxs[p.ID] = primEntry{ctx: primCtx, cancel: primCancel}
			cp.primCtxsMu.Unlock()
			cp.mu.Unlock()
			s.scheduler.RegisterPrimitive(p.ID, scheduler.TypeWaitGroup, p.Name, wg.GetStats())
			s.metricsCollector.RegisterPrimitive(p.ID, p.Type, p.Name)
			restored++
		case string(scheduler.TypeOnce):
			o := primitives.NewOnce()
			cp.mu.Lock()
			cp.onces[p.ID] = o
			primCtx, primCancel := context.WithCancel(s.shutdownCtx)
			cp.primCtxsMu.Lock()
			cp.primCtxs[p.ID] = primEntry{ctx: primCtx, cancel: primCancel}
			cp.primCtxsMu.Unlock()
			cp.mu.Unlock()
			s.scheduler.RegisterPrimitive(p.ID, scheduler.TypeOnce, p.Name, o.GetStats())
			s.metricsCollector.RegisterPrimitive(p.ID, p.Type, p.Name)
			restored++
		case string(scheduler.TypeSingleflight):
			g := primitives.NewGroup()
			cp.mu.Lock()
			cp.singleflights[p.ID] = g
			primCtx, primCancel := context.WithCancel(s.shutdownCtx)
			cp.primCtxsMu.Lock()
			cp.primCtxs[p.ID] = primEntry{ctx: primCtx, cancel: primCancel}
			cp.primCtxsMu.Unlock()
			cp.mu.Unlock()
			s.scheduler.RegisterPrimitive(p.ID, scheduler.TypeSingleflight, p.Name, g.GetStats())
			s.metricsCollector.RegisterPrimitive(p.ID, p.Type, p.Name)
			restored++
		default:
			slog.Warn("snapshot: unknown primitive type, skipping", "type", p.Type, "id", p.ID)
		}
	}
	slog.Info("snapshot loaded", "path", s.snapshotPath, "restored", restored)
}

// jsonMarshal marshals data to JSON
func jsonMarshal(v interface{}) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		slog.Error("Error marshaling JSON", "err", err)
		return json.RawMessage("{}")
	}
	return data
}

// Start starts the web server. It blocks until the server is shut down.
// Use Shutdown to stop it gracefully.
func (s *Server) Start(addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/ws", s.HandleWebSocket)
	mux.HandleFunc("/metrics", s.handlePrometheusMetrics)
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/", s.HandleStatic)

	// Wrap mux to reject path-traversal attempts with 404 before ServeMux
	// can issue its 301 redirect (e.g. GET /..%2Fetc%2Fpasswd → 301 → 404).
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "..") {
			http.NotFound(w, r)
			return
		}
		mux.ServeHTTP(w, r)
	})

	srv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	s.httpServer = srv

	slog.Info("Server starting", "addr", addr)

	// S4: TLS support
	if s.cfg.TLSCertFile != "" && s.cfg.TLSKeyFile != "" {
		if err := srv.ListenAndServeTLS(s.cfg.TLSCertFile, s.cfg.TLSKeyFile); err != nil && err != http.ErrServerClosed {
			return err
		}
		return nil
	}

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

// Shutdown gracefully stops the server and all background goroutines.
// The provided context controls how long to wait for active connections to finish.
func (s *Server) Shutdown(ctx context.Context) error {
	s.draining.Store(true)

	// Notify all active WS clients with a graceful close frame.
	closeMsg := websocket.FormatCloseMessage(websocket.CloseGoingAway, "server shutting down")
	s.clientsMu.RLock()
	clients := make([]*websocket.Conn, 0, len(s.clients))
	for conn := range s.clients {
		clients = append(clients, conn)
	}
	s.clientsMu.RUnlock()
	for _, conn := range clients {
		s.writeMuMu.Lock()
		mu, ok := s.writeMu[conn]
		s.writeMuMu.Unlock()
		if !ok {
			continue
		}
		mu.Lock()
		_ = conn.SetWriteDeadline(time.Now().Add(2 * time.Second))
		_ = conn.WriteMessage(websocket.CloseMessage, closeMsg)
		mu.Unlock()
	}

	// Persist current state before cancelling contexts so all primitives
	// are still reachable when saveSnapshot iterates connPrimsMap (Fix 8).
	s.saveSnapshot()

	// Cancel shutdownCtx first so goroutines blocked on context-aware
	// primitives (LockContext, RLockContext, AcquireContext, etc.) unblock
	// before we wait for HTTP connections to drain.
	s.shutdownCancel()
	// Signal background goroutines in this server to exit.
	close(s.stop)
	// Signal the scheduler (and forwardSchedulerUpdates) to exit.
	s.scheduler.Stop()
	// Drain active HTTP/WebSocket connections.
	if s.httpServer != nil {
		return s.httpServer.Shutdown(ctx)
	}
	return nil
}
