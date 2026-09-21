// Package observability provides the shared logging and progress boundaries
// used by Rescrobble commands.
package observability

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"
)

// Level controls which events a Logger writes. A more verbose selected level
// includes all less verbose levels.
type Level uint8

const (
	LevelNormal Level = iota
	LevelVerbose
	LevelDebug
)

// Short names are provided for callers that prefer the concise terminology
// used by the command line interface.
const (
	Normal  = LevelNormal
	Verbose = LevelVerbose
	Debug   = LevelDebug
)

func (l Level) String() string {
	switch l {
	case LevelNormal:
		return "normal"
	case LevelVerbose:
		return "verbose"
	case LevelDebug:
		return "debug"
	default:
		return fmt.Sprintf("level(%d)", l)
	}
}

// ParseLevel parses a command-line log level.
func ParseLevel(value string) (Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "normal":
		return LevelNormal, nil
	case "verbose":
		return LevelVerbose, nil
	case "debug":
		return LevelDebug, nil
	default:
		return LevelNormal, fmt.Errorf("unknown log level %q (want normal, verbose, or debug)", value)
	}
}

// Allows reports whether selected includes eventLevel.
func (selected Level) Allows(eventLevel Level) bool {
	return eventLevel <= selected
}

// Event is the stable, structured representation written by Logger.
type Event struct {
	Timestamp time.Time      `json:"timestamp"`
	Severity  string         `json:"severity"`
	Name      string         `json:"event"`
	Fields    map[string]any `json:"fields,omitempty"`
}

// Logger writes newline-delimited JSON events. It is safe for concurrent
// command components to share.
type Logger struct {
	mu         sync.Mutex
	writer     io.Writer
	level      Level
	redactions []string
}

// NewLogger creates a structured logger at level. Optional values are
// redacted from every emitted event; this is intended for session
// credentials and other secrets.
func NewLogger(writer io.Writer, level Level, redactions ...string) *Logger {
	logger := &Logger{
		writer: writer,
		level:  level,
	}
	logger.SetRedactions(redactions...)
	return logger
}

// SetLevel changes the selected level for subsequent events.
func (l *Logger) SetLevel(level Level) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.level = level
}

// Level returns the selected level.
func (l *Logger) Level() Level {
	if l == nil {
		return LevelNormal
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.level
}

// SetRedactions replaces the values redacted from subsequent events. Empty
// values are ignored so a missing credential cannot redact unrelated output.
func (l *Logger) SetRedactions(values ...string) {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	l.redactions = l.redactions[:0]
	for _, value := range values {
		if value != "" {
			l.redactions = append(l.redactions, value)
		}
	}
}

// Emit writes an event when its severity is permitted by the selected level.
// A filtered event is successful and produces no output.
func (l *Logger) Emit(severity Level, name string, fields map[string]any) error {
	if l == nil {
		return errors.New("logger is nil")
	}

	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.level.Allows(severity) {
		return nil
	}
	if l.writer == nil {
		return errors.New("logger writer is nil")
	}

	event := Event{
		Timestamp: time.Now().UTC(),
		Severity:  severity.String(),
		Name:      name,
		Fields:    fields,
	}
	data, err := json.Marshal(event)
	if err != nil {
		return fmt.Errorf("encode %s event: %w", name, err)
	}
	data = redactJSON(data, l.redactions)
	data = append(data, '\n')
	if _, err := l.writer.Write(data); err != nil {
		return fmt.Errorf("write %s event: %w", name, err)
	}
	return nil
}

func redactJSON(data []byte, secrets []string) []byte {
	encodedSecrets := make([][]byte, 0, len(secrets))
	marker := "[REDACTED]"
	for _, secret := range secrets {
		encoded, err := json.Marshal(secret)
		if err != nil || len(encoded) < 2 {
			continue
		}
		encodedSecrets = append(encodedSecrets, encoded[1:len(encoded)-1])
		if strings.Contains(marker, secret) {
			marker = ""
		}
	}
	if len(encodedSecrets) == 0 {
		return data
	}

	var redacted bytes.Buffer
	redacted.Grow(len(data))
	for index := 0; index < len(data); {
		if data[index] != '"' {
			redacted.WriteByte(data[index])
			index++
			continue
		}

		start := index
		index++
		for index < len(data) {
			if data[index] == '\\' {
				index += 2
				continue
			}
			if data[index] == '"' {
				index++
				break
			}
			index++
		}

		redacted.WriteByte('"')
		content := data[start+1 : index-1]
		for _, secret := range encodedSecrets {
			content = bytes.ReplaceAll(content, secret, []byte(marker))
		}
		redacted.Write(content)
		redacted.WriteByte('"')
	}
	return redacted.Bytes()
}

// Normal emits a normal-level event.
func (l *Logger) Normal(name string, fields map[string]any) error {
	return l.Emit(LevelNormal, name, fields)
}

// Verbose emits a verbose-level event.
func (l *Logger) Verbose(name string, fields map[string]any) error {
	return l.Emit(LevelVerbose, name, fields)
}

// Debug emits a debug-level event.
func (l *Logger) Debug(name string, fields map[string]any) error {
	return l.Emit(LevelDebug, name, fields)
}
