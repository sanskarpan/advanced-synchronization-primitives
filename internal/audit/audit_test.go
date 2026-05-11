package audit

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoggerWritesEntry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger, err := New(path, 1024*1024, 2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	logger.Log(Entry{
		Timestamp:   time.Now().UTC(),
		Event:       "primitive_op",
		ConnID:      "c1",
		PrimitiveID: "m1",
		Op:          "lock",
		Result:      "success",
	})
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("ReadFile: %v", err)
	}
	var entry Entry
	if err := json.Unmarshal([]byte(strings.TrimSpace(string(data))), &entry); err != nil {
		t.Fatalf("Unmarshal: %v", err)
	}
	if entry.Event != "primitive_op" {
		t.Fatalf("expected primitive_op, got %q", entry.Event)
	}
}

func TestLoggerRotation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	logger, err := New(path, 128, 2)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	for i := 0; i < 16; i++ {
		logger.Log(Entry{
			Timestamp: time.Now().UTC(),
			Event:     "primitive_op",
			ConnID:    "c1",
			Result:    "success",
			Error:     strings.Repeat("x", 32),
		})
	}
	if err := logger.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := os.Stat(path); err != nil {
		t.Fatalf("expected active audit log: %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("expected rotated audit log: %v", err)
	}
}
