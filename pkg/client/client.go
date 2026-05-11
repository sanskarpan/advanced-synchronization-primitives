package client

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/websocket"
)

const (
	defaultReconnectDelay = time.Second
	defaultMaxReconnect   = 30 * time.Second
	defaultPingInterval   = 30 * time.Second
)

// ErrNotConnected is returned when an operation requires an active websocket
// connection but none is available.
var ErrNotConnected = errors.New("client: not connected")

// ErrClosed is returned after Close has been called.
var ErrClosed = errors.New("client: closed")

// Config configures the websocket client.
type Config struct {
	URL            string
	APIKey         string
	ReconnectDelay time.Duration
	MaxReconnect   time.Duration
	PingInterval   time.Duration
	AutoReconnect  bool
}

// ServerError wraps an error message returned by the server.
type ServerError struct {
	Message string
}

func (e *ServerError) Error() string {
	return "client: server error: " + e.Message
}

type message struct {
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type response struct {
	message string
	err     error
}

// Client is a serialized websocket client for the sync primitives server.
type Client struct {
	cfg Config

	sendMu sync.Mutex

	mu      sync.Mutex
	conn    *websocket.Conn
	closed  bool
	pending chan response
}

// New constructs a client with defaults applied.
func New(cfg Config) *Client {
	if cfg.ReconnectDelay <= 0 {
		cfg.ReconnectDelay = defaultReconnectDelay
	}
	if cfg.MaxReconnect <= 0 {
		cfg.MaxReconnect = defaultMaxReconnect
	}
	if cfg.PingInterval <= 0 {
		cfg.PingInterval = defaultPingInterval
	}
	return &Client{cfg: cfg}
}

// Connect establishes the websocket connection and consumes the initial state
// message emitted by the server.
func (c *Client) Connect(ctx context.Context) error {
	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	if err := c.readInitialState(ctx, conn); err != nil {
		_ = conn.Close()
		return err
	}

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		_ = conn.Close()
		return ErrClosed
	}
	old := c.conn
	c.conn = conn
	c.mu.Unlock()

	if old != nil {
		_ = old.Close()
	}
	go c.readLoop(conn)
	return nil
}

// Close terminates the current websocket connection and prevents reconnects.
func (c *Client) Close() error {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	c.closed = true
	conn := c.conn
	c.conn = nil
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()

	if pending != nil {
		select {
		case pending <- response{err: ErrClosed}:
		default:
		}
	}

	if conn != nil {
		_ = conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(250*time.Millisecond),
		)
		return conn.Close()
	}
	return nil
}

func (c *Client) send(ctx context.Context, msgType string, payload interface{}) (string, error) {
	c.sendMu.Lock()
	defer c.sendMu.Unlock()

	conn, err := c.ensureConnected(ctx)
	if err != nil {
		return "", err
	}

	data, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("client: marshal payload: %w", err)
	}

	replyCh := make(chan response, 1)
	c.mu.Lock()
	c.pending = replyCh
	c.mu.Unlock()

	writeDeadline := deadlineFromContext(ctx, 5*time.Second)
	if err := conn.SetWriteDeadline(writeDeadline); err != nil {
		c.clearPending(replyCh)
		return "", fmt.Errorf("client: set write deadline: %w", err)
	}

	if err := conn.WriteJSON(message{Type: msgType, Payload: data}); err != nil {
		c.clearPending(replyCh)
		c.invalidateConn(conn)
		return "", fmt.Errorf("client: send %s: %w", msgType, err)
	}

	select {
	case resp := <-replyCh:
		return resp.message, resp.err
	case <-ctx.Done():
		c.clearPending(replyCh)
		return "", ctx.Err()
	}
}

func (c *Client) ensureConnected(ctx context.Context) (*websocket.Conn, error) {
	c.mu.Lock()
	closed := c.closed
	conn := c.conn
	c.mu.Unlock()

	if closed {
		return nil, ErrClosed
	}
	if conn != nil {
		return conn, nil
	}
	if !c.cfg.AutoReconnect {
		return nil, ErrNotConnected
	}

	delay := c.cfg.ReconnectDelay
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if err := c.Connect(ctx); err == nil {
			c.mu.Lock()
			conn = c.conn
			c.mu.Unlock()
			if conn != nil {
				return conn, nil
			}
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}

		delay *= 2
		if delay > c.cfg.MaxReconnect {
			delay = c.cfg.MaxReconnect
		}
	}
}

func (c *Client) dial(ctx context.Context) (*websocket.Conn, error) {
	if c.cfg.URL == "" {
		return nil, errors.New("client: URL is required")
	}

	dialer := websocket.Dialer{
		HandshakeTimeout: c.cfg.ReconnectDelay,
	}
	headers := http.Header{}
	if c.cfg.APIKey != "" {
		headers.Set("Authorization", "Bearer "+c.cfg.APIKey)
	}

	conn, resp, err := dialer.DialContext(ctx, c.cfg.URL, headers)
	if err != nil {
		if resp != nil {
			return nil, fmt.Errorf("client: dial failed: %v (http %d)", err, resp.StatusCode)
		}
		return nil, fmt.Errorf("client: dial failed: %w", err)
	}
	return conn, nil
}

func (c *Client) readInitialState(ctx context.Context, conn *websocket.Conn) error {
	if err := conn.SetReadDeadline(deadlineFromContext(ctx, 5*time.Second)); err != nil {
		return fmt.Errorf("client: set read deadline: %w", err)
	}
	var msg message
	if err := conn.ReadJSON(&msg); err != nil {
		return fmt.Errorf("client: read initial state: %w", err)
	}
	if msg.Type != "initialState" && msg.Type != "state" {
		return fmt.Errorf("client: expected initialState, got %s", msg.Type)
	}
	return conn.SetReadDeadline(time.Time{})
}

func (c *Client) readLoop(conn *websocket.Conn) {
	for {
		var msg message
		if err := conn.ReadJSON(&msg); err != nil {
			c.fail(conn, fmt.Errorf("client: read failed: %w", err))
			return
		}

		switch msg.Type {
		case "success", "error":
			var payload struct {
				Message string `json:"message"`
			}
			if err := json.Unmarshal(msg.Payload, &payload); err != nil {
				c.fail(conn, fmt.Errorf("client: decode response: %w", err))
				return
			}
			resp := response{message: payload.Message}
			if msg.Type == "error" {
				resp.err = &ServerError{Message: payload.Message}
			}
			c.deliver(resp)
		case "initialState", "state", "update", "metrics":
			continue
		default:
			continue
		}
	}
}

func (c *Client) deliver(resp response) {
	c.mu.Lock()
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()

	if pending != nil {
		select {
		case pending <- resp:
		default:
		}
	}
}

func (c *Client) fail(conn *websocket.Conn, err error) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	pending := c.pending
	c.pending = nil
	c.mu.Unlock()

	if pending != nil {
		select {
		case pending <- response{err: err}:
		default:
		}
	}

	_ = conn.Close()
}

func (c *Client) clearPending(replyCh chan response) {
	c.mu.Lock()
	if c.pending == replyCh {
		c.pending = nil
	}
	c.mu.Unlock()
}

func (c *Client) invalidateConn(conn *websocket.Conn) {
	c.mu.Lock()
	if c.conn == conn {
		c.conn = nil
	}
	c.mu.Unlock()
	_ = conn.Close()
}

func deadlineFromContext(ctx context.Context, fallback time.Duration) time.Time {
	if deadline, ok := ctx.Deadline(); ok {
		return deadline
	}
	return time.Now().Add(fallback)
}
