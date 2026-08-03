package logger_test

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/sbidhya/tessera/internal/logger"
)

func TestNew_JSONByDefault(t *testing.T) {
	var buf bytes.Buffer
	log := logger.New("info", "json", &buf)
	log.Info("hello", "key", "value")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("expected JSON output, got %q: %v", buf.String(), err)
	}
	if m["msg"] != "hello" {
		t.Errorf("msg = %v, want hello", m["msg"])
	}
	if m["key"] != "value" {
		t.Errorf("key = %v, want value", m["key"])
	}
}

func TestNew_TextFormat(t *testing.T) {
	var buf bytes.Buffer
	log := logger.New("info", "text", &buf)
	log.Info("hello text")

	if !strings.Contains(buf.String(), "hello text") {
		t.Errorf("expected text output to contain message, got %q", buf.String())
	}
	// Text format should NOT be JSON
	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err == nil {
		t.Error("expected text format, but output was valid JSON")
	}
}

func TestNew_Levels(t *testing.T) {
	tests := []struct {
		level      string
		debugShown bool
		infoShown  bool
	}{
		{"debug", true, true},
		{"info", false, true},
		{"warn", false, false},
		{"error", false, false},
		{"unknown-garbage", false, true}, // falls back to info
	}

	for _, tc := range tests {
		t.Run(tc.level, func(t *testing.T) {
			var buf bytes.Buffer
			log := logger.New(tc.level, "json", &buf)

			buf.Reset()
			log.Debug("debug msg")
			debugShown := buf.Len() > 0

			buf.Reset()
			log.Info("info msg")
			infoShown := buf.Len() > 0

			if debugShown != tc.debugShown {
				t.Errorf("level %q: debug shown = %v, want %v (buf=%q)", tc.level, debugShown, tc.debugShown, buf.String())
			}
			if infoShown != tc.infoShown {
				t.Errorf("level %q: info shown = %v, want %v", tc.level, infoShown, tc.infoShown)
			}
		})
	}
}

func TestNew_CaseInsensitive(t *testing.T) {
	var buf bytes.Buffer
	// DEBUG and JSON in different cases should still work
	log := logger.New("DEBUG", "JSON", &buf)
	if !log.Enabled(nil, slog.LevelDebug) {
		t.Error("expected DEBUG level to be enabled case-insensitively")
	}

	buf.Reset()
	log = logger.New("INFO", "TEXT", &buf)
	log.Info("hi")
	if !strings.Contains(buf.String(), "hi") {
		t.Errorf("expected text logger to work case-insensitively, got %q", buf.String())
	}
}

func TestNew_NilWriterUsesStdout(t *testing.T) {
	// Should not panic when out is nil.
	log := logger.New("info", "json", nil)
	if log == nil {
		t.Fatal("expected non-nil logger when out is nil")
	}
	// Just ensure it can log without panic (writes to os.Stdout, we don't capture)
	log.Info("nil writer test")
}

func TestNew_UnknownFormatFallsBackToJSON(t *testing.T) {
	var buf bytes.Buffer
	log := logger.New("info", "xml", &buf)
	log.Info("fallback")

	var m map[string]any
	if err := json.Unmarshal(buf.Bytes(), &m); err != nil {
		t.Fatalf("unknown format should fallback to JSON, got %q: %v", buf.String(), err)
	}
}
