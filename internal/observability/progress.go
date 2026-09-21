package observability

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
)

type terminalDetector interface {
	IsTerminal() bool
}

// Progress renders run updates safely for both interactive terminals and
// redirected output. A terminal update overwrites the current line; a
// non-terminal update is always a complete newline-delimited record.
type Progress struct {
	mu       sync.Mutex
	writer   io.Writer
	terminal bool
}

// NewProgress detects terminal capability from writers that implement
// IsTerminal. *os.File values are also detected as character devices.
func NewProgress(writer io.Writer) *Progress {
	terminal := false
	if detector, ok := writer.(terminalDetector); ok {
		terminal = detector.IsTerminal()
	} else if file, ok := writer.(*os.File); ok {
		if info, err := file.Stat(); err == nil {
			terminal = info.Mode()&os.ModeCharDevice != 0
		}
	}
	return NewProgressWithTerminal(writer, terminal)
}

// NewProgressWithTerminal creates a renderer with explicit terminal
// capability. It is useful for callers and tests with an injected writer.
func NewProgressWithTerminal(writer io.Writer, terminal bool) *Progress {
	return &Progress{writer: writer, terminal: terminal}
}

// IsTerminal reports the rendering mode selected for this progress writer.
func (p *Progress) IsTerminal() bool {
	if p == nil {
		return false
	}
	return p.terminal
}

// Update renders one progress state.
func (p *Progress) Update(completed, total int, detail string) error {
	if p == nil {
		return errors.New("progress is nil")
	}
	if completed < 0 || total < 0 {
		return errors.New("progress counts must not be negative")
	}

	line := fmt.Sprintf("progress: %d/%d", completed, total)
	if detail = strings.TrimSpace(sanitizeText(detail)); detail != "" {
		line += " " + detail
	}
	return p.write(line, false)
}

// Complete renders the final summary and terminates the progress line.
func (p *Progress) Complete(summary string) error {
	if p == nil {
		return errors.New("progress is nil")
	}
	summary = strings.TrimSpace(sanitizeText(summary))
	if summary == "" {
		return errors.New("progress summary must not be empty")
	}
	return p.write(summary, true)
}

// Finish is an alias for Complete for callers that use start/update/finish
// terminology.
func (p *Progress) Finish(summary string) error {
	return p.Complete(summary)
}

func (p *Progress) write(line string, complete bool) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.writer == nil {
		return errors.New("progress writer is nil")
	}
	if p.terminal {
		if _, err := io.WriteString(p.writer, "\r\x1b[2K"+line); err != nil {
			return err
		}
		if complete {
			_, err := io.WriteString(p.writer, "\n")
			return err
		}
		return nil
	}
	_, err := io.WriteString(p.writer, line+"\n")
	return err
}

func sanitizeText(value string) string {
	value = stripANSISequences(value)
	return strings.Map(func(r rune) rune {
		if r < 0x20 || (r >= 0x7f && r <= 0x9f) {
			return -1
		}
		return r
	}, value)
}

func stripANSISequences(value string) string {
	var sanitized strings.Builder
	sanitized.Grow(len(value))
	for index := 0; index < len(value); {
		if value[index] != 0x1b && value[index] != 0x9b && value[index] != 0x9d {
			sanitized.WriteByte(value[index])
			index++
			continue
		}

		switch value[index] {
		case 0x9b:
			index++
			index = skipANSICSI(value, index)
		case 0x9d:
			index++
			index = skipANSIOSC(value, index)
		default:
			index++
			if index == len(value) {
				continue
			}
			switch value[index] {
			case '[':
				index = skipANSICSI(value, index+1)
			case ']':
				index = skipANSIOSC(value, index+1)
			default:
				index++
			}
		}
	}
	return sanitized.String()
}

func skipANSICSI(value string, index int) int {
	for index < len(value) {
		// The final byte of a CSI sequence is in the range 0x40-0x7e.
		if value[index] >= 0x40 && value[index] <= 0x7e {
			return index + 1
		}
		index++
	}
	return len(value)
}

func skipANSIOSC(value string, index int) int {
	for index < len(value) {
		switch value[index] {
		case 0x07:
			return index + 1
		case 0x1b:
			if index+1 < len(value) && value[index+1] == '\\' {
				return index + 2
			}
		}
		index++
	}
	return len(value)
}
