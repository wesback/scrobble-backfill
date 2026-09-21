package observability

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestLoggerFiltersNormalVerboseAndDebugLevels(t *testing.T) {
	tests := []struct {
		name     string
		selected Level
		want     []string
	}{
		{name: "normal", selected: LevelNormal, want: []string{"normal-event"}},
		{name: "verbose", selected: LevelVerbose, want: []string{"normal-event", "verbose-event"}},
		{name: "debug", selected: LevelDebug, want: []string{"normal-event", "verbose-event", "debug-event"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			logger := NewLogger(&output, test.selected)
			if err := logger.Normal("normal-event", map[string]any{"count": 1}); err != nil {
				t.Fatalf("normal event: %v", err)
			}
			if err := logger.Verbose("verbose-event", map[string]any{"count": 2}); err != nil {
				t.Fatalf("verbose event: %v", err)
			}
			if err := logger.Debug("debug-event", map[string]any{"count": 3}); err != nil {
				t.Fatalf("debug event: %v", err)
			}

			lines := strings.Split(strings.TrimSpace(output.String()), "\n")
			if len(lines) != len(test.want) {
				t.Fatalf("output lines = %d, want %d: %q", len(lines), len(test.want), output.String())
			}
			for index, line := range lines {
				var event Event
				if err := json.Unmarshal([]byte(line), &event); err != nil {
					t.Fatalf("decode event %d: %v", index, err)
				}
				if event.Name != test.want[index] {
					t.Errorf("event %d name = %q, want %q", index, event.Name, test.want[index])
				}
				if event.Timestamp.IsZero() {
					t.Errorf("event %d has no timestamp", index)
				}
				wantSeverity := []string{"normal", "verbose", "debug"}[index]
				if event.Severity != wantSeverity {
					t.Errorf("event %d severity = %q, want %q", index, event.Severity, wantSeverity)
				}
				if event.Fields["count"] != float64(index+1) {
					t.Errorf("event %d fields = %#v, want count %d", index, event.Fields, index+1)
				}
			}
		})
	}
}

func TestLoggerRedactsConfiguredLastFMSessionCredentials(t *testing.T) {
	const session = "lastfm-session-secret-123"
	var output bytes.Buffer
	logger := NewLogger(&output, LevelDebug, session)

	if err := logger.Debug("authenticated", map[string]any{
		"profile": "personal",
		"session": session,
		"nested":  map[string]any{"message": "credential=" + session},
	}); err != nil {
		t.Fatalf("debug event: %v", err)
	}
	if strings.Contains(output.String(), session) {
		t.Fatalf("event output contains session credential: %q", output.String())
	}
	if !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("event output = %q, want redaction marker", output.String())
	}
	var event Event
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil {
		t.Fatalf("decode redacted event: %v", err)
	}
	if event.Fields["session"] == session {
		t.Fatalf("decoded event contains session credential: %#v", event.Fields)
	}
}

func TestLoggerRedactsJSONEscapedSessionCredentials(t *testing.T) {
	session := "lastfm-\"quoted\"\\line\nbreak"
	var output bytes.Buffer
	logger := NewLogger(&output, LevelDebug, session)

	if err := logger.Debug("authenticated", map[string]any{
		"session": session,
	}); err != nil {
		t.Fatalf("debug event: %v", err)
	}

	encoded, err := json.Marshal(session)
	if err != nil {
		t.Fatalf("encode session: %v", err)
	}
	encoded = encoded[1 : len(encoded)-1]
	if bytes.Contains(output.Bytes(), encoded) {
		t.Fatalf("event output contains encoded session credential: %q", output.String())
	}
	if !strings.Contains(output.String(), "[REDACTED]") {
		t.Fatalf("event output = %q, want redaction marker", output.String())
	}
}

func TestEventTimestampUsesUTC(t *testing.T) {
	var output bytes.Buffer
	if err := NewLogger(&output, LevelNormal).Normal("started", nil); err != nil {
		t.Fatalf("normal event: %v", err)
	}
	var event Event
	if err := json.Unmarshal(bytes.TrimSpace(output.Bytes()), &event); err != nil {
		t.Fatalf("decode event: %v", err)
	}
	if event.Timestamp.Location() != time.UTC {
		t.Fatalf("timestamp location = %v, want UTC", event.Timestamp.Location())
	}
}
