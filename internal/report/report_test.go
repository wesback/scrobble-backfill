package report

import (
	"encoding/csv"
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/wesback/scrobble-backfill/internal/journal"
)

func TestRenderFormatsIncludePortableOutcomesAndRedactCredentials(t *testing.T) {
	started := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	completed := started.Add(1500 * time.Millisecond)
	run := journal.Run{
		Profile: "personal", InvocationID: "import-1",
		CreatedAt: started, UpdatedAt: completed,
		Settings: journal.RunSettings{
			From: started, To: started,
			TimestampTolerance: 25 * time.Second, TimestampToleranceSet: true,
			EligibilityRule: "eligible when at least half the track was played",
			BatchDelay:      250 * time.Millisecond, BatchDelaySet: true,
		},
		Batches: []journal.Batch{{
			Sequence: 1, State: journal.StateSubmitted,
			PlannedAt: started, SubmittedAt: reportTimePtr(completed),
			Payloads: []journal.Submission{{Artist: "Artist", Track: "Track", Timestamp: started}},
		}, {
			Sequence: 2, State: journal.StatePlanned,
			PlannedAt: completed,
			Payloads:  []journal.Submission{{Artist: "Artist", Track: "Retry", Timestamp: started}},
		}},
		Events: []journal.Event{
			{Type: "ingestion.warning", Data: map[string]string{
				"input": "malformed.json", "code": "malformed_record",
				"reason": "=bad input <tag>", "field": "track",
			}},
			{Type: "comparison.excluded", Data: map[string]string{
				"reason": "excluded_podcast", "count": "1",
			}},
			{Type: "comparison.matched", Data: map[string]string{"count": "1"}},
			{Type: "submission.batch.planned", Data: map[string]string{"batch": "1", "count": "1"}},
			{Type: "submission.batch.submitted", Data: map[string]string{"batch": "1", "count": "1"}},
			{Type: "submission.batch.planned", Data: map[string]string{"batch": "2", "count": "1"}},
			{Type: "submission.batch.failed", Data: map[string]string{
				"batch": "2", "message": "request failed: sk=session-secret",
			}},
			{Type: "run.failed", Data: map[string]string{
				"failure_id": "run-1", "related_batch": "2",
				"message": "request failed: sk=session-secret",
			}},
		},
	}

	formats := []Format{FormatJSON, FormatCSV, FormatHTML}
	for _, format := range formats {
		t.Run(string(format), func(t *testing.T) {
			output, err := Render(run, Options{Format: format, Location: time.UTC})
			if err != nil {
				t.Fatalf("render: %v", err)
			}
			text := string(output)
			for _, want := range []string{
				"personal", "2026-09-22", "25s", "250ms",
				"imported_scrobbles", "skipped_duplicates", "failures",
				"warnings", "metadata_issues", "batch_count", "execution_timing",
				"malformed_record", "excluded_podcast", "session-secret",
			} {
				if want == "session-secret" {
					if strings.Contains(text, want) {
						t.Fatalf("report contains credential %q: %s", want, text)
					}
					continue
				}
				if !strings.Contains(text, want) {
					t.Fatalf("report = %q, want %q", text, want)
				}
			}
			document := Build(run, Options{Format: format, Location: time.UTC})
			if document.Counts.ImportedScrobbles != 1 ||
				document.Counts.SkippedDuplicates != 1 ||
				document.Counts.Failures != 1 ||
				document.Counts.Warnings != 1 ||
				document.Counts.MetadataIssues != 1 ||
				document.Counts.BatchCount != 2 ||
				document.Counts.Excluded["excluded_podcast"] != 1 {
				t.Fatalf("counts = %#v", document.Counts)
			}
			if format == FormatJSON {
				var decoded Document
				if err := json.Unmarshal(output, &decoded); err != nil {
					t.Fatalf("decode JSON: %v", err)
				}
				if !reflect.DeepEqual(decoded.DateRange, DateRange{From: "2026-09-22", To: "2026-09-22"}) {
					t.Fatalf("date range = %#v", decoded.DateRange)
				}
			}
			if format == FormatCSV {
				rows, err := csv.NewReader(strings.NewReader(text)).ReadAll()
				if err != nil {
					t.Fatalf("decode CSV: %v", err)
				}
				for _, row := range rows {
					if len(row) >= 2 && row[0] == "warning" && strings.HasPrefix(row[1], "=") {
						t.Fatalf("CSV formula injection was not neutralized: %#v", row)
					}
				}
			}
			if format == FormatHTML && strings.Contains(text, `\n`) {
				t.Fatalf("HTML contains literal backslash-n: %q", text)
			}
		})
	}
}

func TestBuildReportsAPIAcceptedCountAndIgnoredReason(t *testing.T) {
	started := time.Date(2026, 9, 22, 10, 0, 0, 0, time.UTC)
	run := journal.Run{
		Profile: "personal", InvocationID: "import-partial",
		Batches: []journal.Batch{{
			Sequence: 1, State: journal.StateSubmitted,
			Payloads: []journal.Submission{
				{Artist: "Artist", Track: "Accepted", Timestamp: started},
				{Artist: "Artist", Track: "Ignored", Timestamp: started.Add(time.Minute)},
			},
		}},
		Events: []journal.Event{
			{Type: "submission.batch.result", Data: map[string]string{
				"batch": "1", "accepted": "1", "ignored": "1", "count": "2",
			}},
			{Type: "submission.batch.submitted", Data: map[string]string{
				"batch": "1", "count": "2",
			}},
			{Type: "submission.scrobble.ignored", Data: map[string]string{
				"batch": "1", "index": "2", "artist": "Artist", "track": "Ignored",
				"code": "1", "reason": "Timestamp is too old",
			}},
		},
	}
	document := Build(run, Options{})
	if document.Counts.ImportedScrobbles != 1 || document.Counts.IgnoredScrobbles != 1 {
		t.Fatalf("counts = %#v, want one API-accepted and one ignored scrobble", document.Counts)
	}
	if len(document.Ignored) != 1 || document.Ignored[0].Track != "Ignored" ||
		document.Ignored[0].Reason != "Timestamp is too old" {
		t.Fatalf("ignored diagnostics = %#v, want track and Last.fm reason", document.Ignored)
	}

	output, err := Render(run, Options{Format: FormatJSON})
	if err != nil {
		t.Fatalf("render JSON report: %v", err)
	}
	var decoded Document
	if err := json.Unmarshal(output, &decoded); err != nil {
		t.Fatalf("decode JSON report: %v", err)
	}
	if decoded.Counts.ImportedScrobbles != 1 || decoded.Counts.IgnoredScrobbles != 1 ||
		len(decoded.Ignored) != 1 || decoded.Ignored[0].Reason != "Timestamp is too old" {
		t.Fatalf("decoded report = %#v, want accepted count and ignored reason", decoded)
	}
}

func reportTimePtr(value time.Time) *time.Time {
	return &value
}
