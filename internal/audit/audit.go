package audit

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"
)

const defaultBufferSize = 4096

// Entry is a single NDJSON audit record.
type Entry struct {
	Timestamp     time.Time `json:"ts"`
	Event         string    `json:"event"`
	ConnID        string    `json:"conn_id,omitempty"`
	User          string    `json:"user,omitempty"`
	PrimitiveID   string    `json:"primitive_id,omitempty"`
	PrimitiveType string    `json:"primitive_type,omitempty"`
	Op            string    `json:"op,omitempty"`
	HoldMs        int       `json:"hold_ms,omitempty"`
	Result        string    `json:"result,omitempty"`
	DurationNs    int64     `json:"duration_ns,omitempty"`
	Error         string    `json:"error,omitempty"`
}

// Logger asynchronously writes audit entries to NDJSON with optional rotation.
type Logger struct {
	path      string
	maxBytes  int64
	keepFiles int
	ch        chan Entry
	done      chan struct{}
	wg        sync.WaitGroup

	mu           sync.Mutex
	file         *os.File
	currentBytes int64
	dropped      atomic.Int64
}

// New creates and starts an audit logger.
func New(path string, maxBytes int64, keepFiles int) (*Logger, error) {
	if path == "" {
		return nil, fmt.Errorf("audit: path is required")
	}
	if keepFiles < 0 {
		keepFiles = 0
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("audit: mkdir: %w", err)
	}

	// #nosec G304 -- path is server-controlled configuration
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, fmt.Errorf("audit: open file: %w", err)
	}
	info, err := f.Stat()
	if err != nil {
		_ = f.Close()
		return nil, fmt.Errorf("audit: stat file: %w", err)
	}

	l := &Logger{
		path:         path,
		maxBytes:     maxBytes,
		keepFiles:    keepFiles,
		ch:           make(chan Entry, defaultBufferSize),
		done:         make(chan struct{}),
		file:         f,
		currentBytes: info.Size(),
	}
	l.wg.Add(1)
	go l.loop()
	slog.Info("audit log started", "path", path)
	return l, nil
}

// Log queues an audit entry and drops it if the buffer is full.
func (l *Logger) Log(entry Entry) {
	select {
	case l.ch <- entry:
	default:
		dropped := l.dropped.Add(1)
		slog.Warn("audit log channel full; dropping event", "dropped_total", dropped)
	}
}

// Close drains pending entries and closes the audit file.
func (l *Logger) Close() error {
	close(l.done)
	l.wg.Wait()

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	err := l.file.Close()
	l.file = nil
	return err
}

// Dropped returns the number of dropped audit events.
func (l *Logger) Dropped() int64 {
	return l.dropped.Load()
}

func (l *Logger) loop() {
	defer l.wg.Done()
	for {
		select {
		case entry := <-l.ch:
			l.writeEntry(entry)
		case <-l.done:
			for {
				select {
				case entry := <-l.ch:
					l.writeEntry(entry)
				default:
					return
				}
			}
		}
	}
}

func (l *Logger) writeEntry(entry Entry) {
	data, err := json.Marshal(entry)
	if err != nil {
		slog.Error("audit log marshal error", "err", err)
		return
	}
	data = append(data, '\n')

	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return
	}

	n, err := l.file.Write(data)
	if err != nil {
		slog.Error("audit log write error", "err", err)
		return
	}
	l.currentBytes += int64(n)

	if l.maxBytes > 0 && l.currentBytes >= l.maxBytes {
		if err := l.rotateLocked(); err != nil {
			slog.Error("audit log rotate error", "err", err)
		}
	}
}

func (l *Logger) rotateLocked() error {
	if l.file == nil {
		return nil
	}
	if err := l.file.Close(); err != nil {
		return err
	}

	if l.keepFiles > 0 {
		oldest := fmt.Sprintf("%s.%d", l.path, l.keepFiles)
		_ = os.Remove(oldest)
		for i := l.keepFiles - 1; i >= 1; i-- {
			src := fmt.Sprintf("%s.%d", l.path, i)
			dst := fmt.Sprintf("%s.%d", l.path, i+1)
			if _, err := os.Stat(src); err == nil {
				if err := os.Rename(src, dst); err != nil {
					return err
				}
			}
		}
		if err := os.Rename(l.path, l.path+".1"); err != nil && !os.IsNotExist(err) {
			return err
		}
	} else {
		if err := os.Remove(l.path); err != nil && !os.IsNotExist(err) {
			return err
		}
	}

	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	l.file = f
	l.currentBytes = 0
	slog.Info("audit log rotated", "path", l.path)
	return nil
}
