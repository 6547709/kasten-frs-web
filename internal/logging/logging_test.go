package logging

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestNew_JSONHandler(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, "info")

	logger.Info("hello", "key", "value")

	out := buf.String()
	if !strings.HasPrefix(out, "{") {
		t.Fatalf("output not JSON: %q", out)
	}
	var m map[string]any
	if err := json.Unmarshal([]byte(strings.TrimSpace(out)), &m); err != nil {
		t.Fatalf("unmarshal: %v: %q", err, out)
	}
	if m["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", m["msg"])
	}
	if m["key"] != "value" {
		t.Errorf("key = %v, want value", m["key"])
	}
	if m["level"] != "INFO" {
		t.Errorf("level = %v, want INFO", m["level"])
	}
}

func TestNew_LevelDebug(t *testing.T) {
	var buf bytes.Buffer
	logger := New(&buf, "debug")
	logger.Debug("d")
	if !strings.Contains(buf.String(), "\"msg\":\"d\"") {
		t.Errorf("debug not emitted: %q", buf.String())
	}
}

func TestContextLogger(t *testing.T) {
	var buf bytes.Buffer
	base := New(&buf, "info")
	ctx := WithRequestID(context.Background(), "abc-123")
	FromContext(ctx, base).Info("hi")

	if !strings.Contains(buf.String(), "abc-123") {
		t.Errorf("request id missing: %q", buf.String())
	}
}
