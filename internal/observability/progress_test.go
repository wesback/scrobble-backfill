package observability

import (
	"bytes"
	"strings"
	"testing"
)

type fakeProgressWriter struct {
	bytes.Buffer
	terminal bool
}

func (w *fakeProgressWriter) IsTerminal() bool {
	return w.terminal
}

func TestProgressRendersInPlaceOnTerminal(t *testing.T) {
	writer := &fakeProgressWriter{terminal: true}
	progress := NewProgress(writer)
	if err := progress.Update(1, 2, "reading"); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if err := progress.Update(2, 2, "done"); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if err := progress.Complete("completed 2 items"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	output := writer.String()
	if !strings.Contains(output, "\r") {
		t.Fatalf("terminal output = %q, want carriage-return updates", output)
	}
	if !strings.HasSuffix(output, "completed 2 items\n") {
		t.Fatalf("terminal output = %q, want final summary", output)
	}
}

func TestProgressUsesPlainLinesForNonTerminalWriter(t *testing.T) {
	writer := &fakeProgressWriter{}
	progress := NewProgress(writer)
	if err := progress.Update(1, 2, "reading"); err != nil {
		t.Fatalf("first update: %v", err)
	}
	if err := progress.Update(2, 2, "done"); err != nil {
		t.Fatalf("second update: %v", err)
	}
	if err := progress.Complete("completed 2 items"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	output := writer.String()
	if strings.ContainsAny(output, "\r\x1b") {
		t.Fatalf("non-terminal output contains control characters: %q", output)
	}
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) != 3 {
		t.Fatalf("non-terminal output has %d lines, want 3: %q", len(lines), output)
	}
	if lines[0] != "progress: 1/2 reading" || lines[1] != "progress: 2/2 done" || lines[2] != "completed 2 items" {
		t.Fatalf("non-terminal output = %q, want newline-delimited updates and summary", output)
	}
}

func TestProgressRendersKnownTotalForNonTerminalWriter(t *testing.T) {
	writer := &fakeProgressWriter{}
	progress := NewProgress(writer)
	if err := progress.Update(3, 5, "items"); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := writer.String(); got != "progress: 3/5 items\n" {
		t.Fatalf("output = %q, want %q", got, "progress: 3/5 items\n")
	}
}

func TestProgressRendersUnknownTotalForNonTerminalWriter(t *testing.T) {
	writer := &fakeProgressWriter{}
	progress := NewProgress(writer)
	if err := progress.Update(1000, 0, "records ingested"); err != nil {
		t.Fatalf("update: %v", err)
	}

	if got := strings.TrimSuffix(writer.String(), "\n"); got != "progress: 1000/unknown records ingested" {
		t.Fatalf("output = %q, want %q", got, "progress: 1000/unknown records ingested")
	}
}

func TestProgressRemovesControlCharactersForNonTerminalWriter(t *testing.T) {
	writer := &fakeProgressWriter{}
	progress := NewProgress(writer)
	if err := progress.Update(1, 1, "reading\x1b[2K"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := progress.Complete("done\r\n"); err != nil {
		t.Fatalf("complete: %v", err)
	}
	if strings.ContainsAny(writer.String(), "\r\x1b") {
		t.Fatalf("non-terminal output contains control characters: %q", writer.String())
	}
}

func TestProgressSanitizesTerminalPayloads(t *testing.T) {
	writer := &fakeProgressWriter{terminal: true}
	progress := NewProgress(writer)
	if err := progress.Update(1, 1, "reading \x1b[31msecret\x1b[0m\r\n"); err != nil {
		t.Fatalf("update: %v", err)
	}
	if err := progress.Complete("done \x1b]0;attacker\x07\r\n"); err != nil {
		t.Fatalf("complete: %v", err)
	}

	output := writer.String()
	if strings.Contains(output, "\x1b[31m") || strings.Contains(output, "\x1b[0m") || strings.Contains(output, "\x1b]0;attacker\x07") {
		t.Fatalf("terminal output contains unsanitized escape sequence: %q", output)
	}
	if !strings.Contains(output, "progress: 1/1 reading secret") {
		t.Fatalf("terminal output = %q, want sanitized detail", output)
	}
	if !strings.HasSuffix(output, "done\n") {
		t.Fatalf("terminal output = %q, want sanitized final summary", output)
	}
}
