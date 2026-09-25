package cli

import (
	"errors"
	"strings"
	"testing"

	"github.com/wesback/scrobble-backfill/internal/spotify"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("stderr closed") }

func podcastWarnings(p *warningPrinter, n int) {
	for i := 0; i < n; i++ {
		p.Print(spotify.Warning{Code: spotify.CodePodcast, Reason: "record contains Spotify episode metadata"})
	}
}

func TestWarningPrinterAggregatesRepeatedCodes(t *testing.T) {
	var out strings.Builder
	p := newWarningPrinter(&out, false, false)
	podcastWarnings(p, 100)
	if err := p.Flush(); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	want := []string{
		"warning: excluded_podcast: record contains Spotify episode metadata",
		"warning: excluded_podcast: 100 records",
	}
	if len(lines) != 2 || lines[0] != want[0] || lines[1] != want[1] {
		t.Fatalf("output = %q, want %q", lines, want)
	}
}

func TestWarningPrinterSingleOccurrenceHasNoCountLine(t *testing.T) {
	var out strings.Builder
	p := newWarningPrinter(&out, false, false)
	podcastWarnings(p, 1)
	_ = p.Flush()
	if got := strings.Count(out.String(), "\n"); got != 1 {
		t.Fatalf("output = %q, want 1 line", out.String())
	}
}

func TestWarningPrinterVerbosePrintsEveryRecord(t *testing.T) {
	var out strings.Builder
	p := newWarningPrinter(&out, false, true)
	podcastWarnings(p, 100)
	_ = p.Flush()
	if got := strings.Count(out.String(), "warning: excluded_podcast"); got != 100 {
		t.Fatalf("got %d warning lines, want 100", got)
	}
	if strings.Contains(out.String(), "100 records") {
		t.Fatalf("verbose output has aggregate line: %q", out.String())
	}
}

func TestWarningPrinterClearsProgressLineOnTerminal(t *testing.T) {
	var out strings.Builder
	p := newWarningPrinter(&out, true, false)
	podcastWarnings(p, 1)
	if !strings.HasPrefix(out.String(), "\r\x1b[2Kwarning: ") {
		t.Fatalf("output = %q, want progress line cleared first", out.String())
	}
}

func TestWarningPrinterReportsWriteError(t *testing.T) {
	p := newWarningPrinter(failingWriter{}, false, false)
	podcastWarnings(p, 3)
	if err := p.Flush(); err == nil {
		t.Fatal("Flush() = nil, want write error")
	}
}
