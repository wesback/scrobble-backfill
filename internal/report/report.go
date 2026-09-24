// Package report renders the portable, non-secret outcome of an import run.
package report

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/wesback/scrobble-backfill/internal/journal"
)

type Format string

const (
	FormatJSON Format = "json"
	FormatCSV  Format = "csv"
	FormatHTML Format = "html"
)

// Options controls rendering. Secrets are accepted only to redact legacy or
// manually-authored journal events; they are never emitted.
type Options struct {
	Format                Format
	From                  time.Time
	To                    time.Time
	TimestampTolerance    time.Duration
	TimestampToleranceSet bool
	BatchDelay            time.Duration
	BatchDelaySet         bool
	Location              *time.Location
	Secrets               []string
}

type DateRange struct {
	From string `json:"from"`
	To   string `json:"to"`
}

type ExecutionTiming struct {
	StartedAt   time.Time     `json:"started_at"`
	CompletedAt time.Time     `json:"completed_at"`
	Duration    time.Duration `json:"duration"`
	Elapsed     string        `json:"elapsed"`
}

type Counts struct {
	ImportedScrobbles int            `json:"imported_scrobbles"`
	IgnoredScrobbles  int            `json:"ignored_scrobbles"`
	SkippedDuplicates int            `json:"skipped_duplicates"`
	Failures          int            `json:"failures"`
	Warnings          int            `json:"warnings"`
	MetadataIssues    int            `json:"metadata_issues"`
	BatchCount        int            `json:"batch_count"`
	Eligible          int            `json:"eligible"`
	Excluded          map[string]int `json:"excluded,omitempty"`
}

type Diagnostic struct {
	Type    string `json:"type"`
	Input   string `json:"input,omitempty"`
	Record  string `json:"record,omitempty"`
	Artist  string `json:"artist,omitempty"`
	Track   string `json:"track,omitempty"`
	Code    string `json:"code,omitempty"`
	Reason  string `json:"reason,omitempty"`
	Field   string `json:"field,omitempty"`
	Batch   string `json:"batch,omitempty"`
	Message string `json:"message,omitempty"`
}

// Document is the stable report shape shared by all renderers.
type Document struct {
	Version            int             `json:"version"`
	Profile            string          `json:"profile"`
	InvocationID       string          `json:"invocation_id"`
	DateRange          DateRange       `json:"date_range"`
	TimestampTolerance string          `json:"timestamp_tolerance"`
	EligibilityRule    string          `json:"eligibility_rule"`
	BatchDelay         string          `json:"batch_delay"`
	Counts             Counts          `json:"counts"`
	ExecutionTiming    ExecutionTiming `json:"execution_timing"`
	Warnings           []Diagnostic    `json:"warnings,omitempty"`
	MetadataIssues     []Diagnostic    `json:"metadata_issues,omitempty"`
	Exclusions         []Diagnostic    `json:"exclusions,omitempty"`
	Ignored            []Diagnostic    `json:"ignored,omitempty"`
	Failures           []Diagnostic    `json:"failures,omitempty"`
	Events             []journal.Event `json:"events,omitempty"`
}

var sensitiveAssignment = regexp.MustCompile(`(?i)(session[_ -]?key|credential|password|access[_ -]?token|api[_ -]?key|\bsk)\s*[:=]\s*[^\s,;]+`)

// Build converts a journal run into a report document without exposing
// credentials or provider request details.
func Build(run journal.Run, options Options) Document {
	location := options.Location
	if location == nil {
		location = time.Local
	}
	settings := run.Settings
	from := settings.From
	to := settings.To
	if !options.From.IsZero() {
		from = options.From
	}
	if !options.To.IsZero() {
		to = options.To
	}
	dateRange := DateRange{}
	if !from.IsZero() {
		dateRange.From = from.Format("2006-01-02")
	}
	if !to.IsZero() {
		dateRange.To = to.Format("2006-01-02")
	}
	if dateRange.From == "" || dateRange.To == "" {
		derivedFrom, derivedTo := deriveDateRange(run.Events, location)
		if dateRange.From == "" {
			dateRange.From = derivedFrom
		}
		if dateRange.To == "" {
			dateRange.To = derivedTo
		}
	}

	tolerance := settings.TimestampTolerance
	if options.TimestampToleranceSet {
		tolerance = options.TimestampTolerance
	}
	delay := settings.BatchDelay
	if options.BatchDelaySet {
		delay = options.BatchDelay
	}
	rule := settings.EligibilityRule
	if rule == "" {
		rule = "eligible when the play satisfies Last.fm's scrobble eligibility rule"
	}

	document := Document{
		Version:            1,
		Profile:            sanitize(run.Profile, options.Secrets),
		InvocationID:       sanitize(run.InvocationID, options.Secrets),
		DateRange:          dateRange,
		TimestampTolerance: tolerance.String(),
		EligibilityRule:    sanitize(rule, options.Secrets),
		BatchDelay:         delay.String(),
		Counts:             Counts{Excluded: make(map[string]int)},
		ExecutionTiming: ExecutionTiming{
			StartedAt:   run.CreatedAt,
			CompletedAt: run.UpdatedAt,
		},
	}
	if !document.ExecutionTiming.StartedAt.IsZero() && !document.ExecutionTiming.CompletedAt.IsZero() {
		document.ExecutionTiming.Duration = document.ExecutionTiming.CompletedAt.Sub(document.ExecutionTiming.StartedAt)
		if document.ExecutionTiming.Duration < 0 {
			document.ExecutionTiming.Duration = 0
		}
		document.ExecutionTiming.Elapsed = document.ExecutionTiming.Duration.String()
	}

	seenBatches := make(map[string]struct{})
	failureKeys := make(map[string]struct{})
	resultBatches := make(map[string]struct{})
	for _, event := range run.Events {
		if event.Type == "submission.batch.result" {
			resultBatches[batchKey(event.Data, event.Type)] = struct{}{}
		}
	}
	events := make([]journal.Event, 0, len(run.Events))
	for index, event := range run.Events {
		event = sanitizeEvent(event, options.Secrets)
		events = append(events, event)
		data := event.Data
		switch event.Type {
		case "comparison.matched":
			document.Counts.SkippedDuplicates += parseCount(data["count"], 1)
		case "comparison.eligible":
			document.Counts.Eligible += parseCount(data["count"], 1)
		case "comparison.excluded":
			reason := data["reason"]
			if reason == "" {
				reason = "unknown"
			}
			document.Counts.Excluded[reason] += parseCount(data["count"], 1)
			document.Exclusions = append(document.Exclusions, diagnosticFromEvent(event))
		case "ingestion.warning":
			document.Counts.Warnings += parseCount(data["count"], 1)
			diagnostic := diagnosticFromEvent(event)
			document.Warnings = append(document.Warnings, diagnostic)
			if isMetadataIssue(data["code"]) {
				document.Counts.MetadataIssues += parseCount(data["count"], 1)
				document.MetadataIssues = append(document.MetadataIssues, diagnostic)
			}
		case "submission.batch.planned":
			key := batchKey(data, event.Type)
			seenBatches[key] = struct{}{}
		case "submission.batch.submitted":
			key := batchKey(data, event.Type)
			seenBatches[key] = struct{}{}
			if _, hasResult := resultBatches[key]; !hasResult {
				document.Counts.ImportedScrobbles += parseCount(data["count"], 0)
			}
		case "submission.batch.result":
			key := batchKey(data, event.Type)
			seenBatches[key] = struct{}{}
			document.Counts.ImportedScrobbles += parseCount(data["accepted"], 0)
		case "submission.scrobble.ignored":
			document.Counts.IgnoredScrobbles++
			document.Ignored = append(document.Ignored, diagnosticFromEvent(event))
		case "submission.batch.failed":
			key := batchKey(data, event.Type)
			seenBatches[key] = struct{}{}
			failureKey := failureKeyFor(event, index)
			if _, exists := failureKeys[failureKey]; !exists {
				failureKeys[failureKey] = struct{}{}
				document.Counts.Failures++
				document.Failures = append(document.Failures, diagnosticFromEvent(event))
			}
		case "run.failed":
			if event.Data["related_batch"] == "" && event.Data["message"] != "" &&
				hasFailureMessage(document.Failures, event.Data["message"]) {
				continue
			}
			failureKey := failureKeyFor(event, index)
			if _, exists := failureKeys[failureKey]; !exists {
				failureKeys[failureKey] = struct{}{}
				document.Counts.Failures++
				document.Failures = append(document.Failures, diagnosticFromEvent(event))
			}
		}
	}
	for _, batch := range run.Batches {
		seenBatches[strconv.Itoa(batch.Sequence)] = struct{}{}
		if batch.State == journal.StateSubmitted {
			if _, hasResult := resultBatches[strconv.Itoa(batch.Sequence)]; hasResult {
				continue
			}
			found := false
			for _, event := range run.Events {
				if event.Type == "submission.batch.submitted" && event.Data["batch"] == strconv.Itoa(batch.Sequence) {
					found = true
					break
				}
			}
			if !found {
				document.Counts.ImportedScrobbles += len(batch.Payloads)
			}
		}
	}
	document.Counts.BatchCount = len(seenBatches)
	if len(document.Counts.Excluded) == 0 {
		document.Counts.Excluded = nil
	}
	document.Events = events
	return document
}

// Render builds the selected representation.
func Render(run journal.Run, options Options) ([]byte, error) {
	document := Build(run, options)
	switch options.Format {
	case FormatJSON:
		data, err := json.MarshalIndent(document, "", "  ")
		if err != nil {
			return nil, fmt.Errorf("encode JSON report: %w", err)
		}
		return append(data, '\n'), nil
	case FormatCSV:
		return renderCSV(document)
	case FormatHTML:
		return renderHTML(document), nil
	default:
		return nil, errors.New("report format must be json, csv, or html")
	}
}

func renderCSV(document Document) ([]byte, error) {
	var buffer bytes.Buffer
	writer := csv.NewWriter(&buffer)
	if err := writer.Write([]string{"field", "value", "detail"}); err != nil {
		return nil, err
	}
	rows := [][3]string{
		{"profile", document.Profile, ""},
		{"invocation_id", document.InvocationID, ""},
		{"date_from", document.DateRange.From, ""},
		{"date_to", document.DateRange.To, ""},
		{"timestamp_tolerance", document.TimestampTolerance, ""},
		{"eligibility_rule", document.EligibilityRule, ""},
		{"batch_delay", document.BatchDelay, ""},
		{"imported_scrobbles", strconv.Itoa(document.Counts.ImportedScrobbles), ""},
		{"ignored_scrobbles", strconv.Itoa(document.Counts.IgnoredScrobbles), ""},
		{"skipped_duplicates", strconv.Itoa(document.Counts.SkippedDuplicates), ""},
		{"failures", strconv.Itoa(document.Counts.Failures), ""},
		{"warnings", strconv.Itoa(document.Counts.Warnings), ""},
		{"metadata_issues", strconv.Itoa(document.Counts.MetadataIssues), ""},
		{"batch_count", strconv.Itoa(document.Counts.BatchCount), ""},
		{"eligible", strconv.Itoa(document.Counts.Eligible), ""},
		{"execution_timing", document.ExecutionTiming.Elapsed, document.ExecutionTiming.StartedAt.Format(time.RFC3339Nano) + " to " + document.ExecutionTiming.CompletedAt.Format(time.RFC3339Nano)},
	}
	reasons := make([]string, 0, len(document.Counts.Excluded))
	for reason := range document.Counts.Excluded {
		reasons = append(reasons, reason)
	}
	sort.Strings(reasons)
	for _, reason := range reasons {
		rows = append(rows, [3]string{"excluded", reason, strconv.Itoa(document.Counts.Excluded[reason])})
	}
	appendDiagnostics := func(field string, diagnostics []Diagnostic) {
		for _, diagnostic := range diagnostics {
			data, _ := json.Marshal(diagnostic)
			rows = append(rows, [3]string{field, diagnostic.Reason, string(data)})
		}
	}
	appendDiagnostics("warning", document.Warnings)
	appendDiagnostics("metadata_issue", document.MetadataIssues)
	appendDiagnostics("exclusion", document.Exclusions)
	appendDiagnostics("ignored", document.Ignored)
	appendDiagnostics("failure", document.Failures)
	for _, row := range rows {
		protected := []string{csvSafe(row[0]), csvSafe(row[1]), csvSafe(row[2])}
		if err := writer.Write(protected); err != nil {
			return nil, err
		}
	}
	writer.Flush()
	if err := writer.Error(); err != nil {
		return nil, err
	}
	return buffer.Bytes(), nil
}

func renderHTML(document Document) []byte {
	var builder strings.Builder
	builder.WriteString("<!doctype html>\n<html><head><meta charset=\"utf-8\"><title>Rescrobble report</title></head><body>\n")
	fmt.Fprintf(&builder, "<h1>Rescrobble report for %s</h1>\n", html.EscapeString(document.Profile))
	builder.WriteString("<table><tbody>\n")
	htmlRow := func(name, value string) {
		field := strings.ToLower(strings.ReplaceAll(name, " ", "_"))
		fmt.Fprintf(&builder, "<tr data-field=\"%s\"><th>%s</th><td>%s</td></tr>\n",
			html.EscapeString(field), html.EscapeString(name), html.EscapeString(value))
	}
	htmlRow("Invocation", document.InvocationID)
	htmlRow("Date range", document.DateRange.From+" to "+document.DateRange.To)
	htmlRow("Timestamp tolerance", document.TimestampTolerance)
	htmlRow("Eligibility rule", document.EligibilityRule)
	htmlRow("Batch delay", document.BatchDelay)
	htmlRow("Imported scrobbles", strconv.Itoa(document.Counts.ImportedScrobbles))
	htmlRow("Ignored scrobbles", strconv.Itoa(document.Counts.IgnoredScrobbles))
	htmlRow("Skipped duplicates", strconv.Itoa(document.Counts.SkippedDuplicates))
	htmlRow("Failures", strconv.Itoa(document.Counts.Failures))
	htmlRow("Warnings", strconv.Itoa(document.Counts.Warnings))
	htmlRow("Metadata issues", strconv.Itoa(document.Counts.MetadataIssues))
	htmlRow("Batch count", strconv.Itoa(document.Counts.BatchCount))
	htmlRow("Execution timing", document.ExecutionTiming.Elapsed)
	builder.WriteString("</tbody></table>\n")
	htmlSection := func(title string, diagnostics []Diagnostic) {
		if len(diagnostics) == 0 {
			return
		}
		fmt.Fprintf(&builder, "<h2>%s</h2><ul>\n", html.EscapeString(title))
		for _, diagnostic := range diagnostics {
			data, _ := json.Marshal(diagnostic)
			fmt.Fprintf(&builder, "<li>%s</li>\n", html.EscapeString(string(data)))
		}
		builder.WriteString("</ul>\n")
	}
	htmlSection("Warnings", document.Warnings)
	htmlSection("Metadata issues", document.MetadataIssues)
	htmlSection("Exclusions", document.Exclusions)
	htmlSection("Ignored", document.Ignored)
	htmlSection("Failures", document.Failures)
	builder.WriteString("</body></html>\n")
	return []byte(builder.String())
}

func diagnosticFromEvent(event journal.Event) Diagnostic {
	data := event.Data
	return Diagnostic{
		Type: event.Type, Input: data["input"], Record: data["record"],
		Artist: data["artist"], Track: data["track"],
		Code: data["code"], Reason: data["reason"], Field: data["field"],
		Batch: data["batch"], Message: data["message"],
	}
}

func sanitizeEvent(event journal.Event, secrets []string) journal.Event {
	data := event.Data
	event.Data = make(map[string]string, len(event.Data))
	for key, value := range data {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "credential") || strings.Contains(lower, "session") ||
			strings.Contains(lower, "password") || strings.Contains(lower, "token") ||
			strings.Contains(lower, "secret") {
			event.Data[key] = "[redacted]"
			continue
		}
		event.Data[key] = sanitize(value, secrets)
	}
	return event
}

func sanitize(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	lower := strings.ToLower(value)
	if strings.Contains(lower, "session") &&
		(strings.Contains(lower, "secret") || strings.Contains(lower, "key")) {
		return "[redacted]"
	}
	return sensitiveAssignment.ReplaceAllString(value, "$1=[redacted]")
}

func csvSafe(value string) string {
	if value != "" && strings.ContainsRune("=+-@", rune(value[0])) {
		return "'" + value
	}
	return value
}

func parseCount(value string, fallback int) int {
	if value == "" {
		return fallback
	}
	count, err := strconv.Atoi(value)
	if err != nil || count < 0 {
		return fallback
	}
	return count
}

func batchKey(data map[string]string, eventType string) string {
	if batch := data["batch"]; batch != "" {
		return batch
	}
	return eventType + ":" + data["sequence"]
}

func failureKeyFor(event journal.Event, index int) string {
	if related := event.Data["related_batch"]; related != "" {
		return "batch:" + related
	}
	if id := event.Data["failure_id"]; id != "" {
		return id
	}
	if batch := event.Data["batch"]; batch != "" {
		return "batch:" + batch
	}
	return event.Type + ":" + strconv.Itoa(index)
}

func hasFailureMessage(failures []Diagnostic, message string) bool {
	for _, failure := range failures {
		if failure.Message == message {
			return true
		}
	}
	return false
}

func deriveDateRange(events []journal.Event, location *time.Location) (string, string) {
	var first, last time.Time
	for _, event := range events {
		value := event.Data["timestamp"]
		if value == "" {
			continue
		}
		timestamp, err := time.Parse(time.RFC3339Nano, value)
		if err != nil {
			continue
		}
		local := timestamp.In(location)
		day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
		if first.IsZero() || day.Before(first) {
			first = day
		}
		if last.IsZero() || day.After(last) {
			last = day
		}
	}
	if first.IsZero() {
		return "", ""
	}
	return first.Format("2006-01-02"), last.Format("2006-01-02")
}

func isMetadataIssue(code string) bool {
	switch code {
	case "missing_required_field", "malformed_record", "corrupt_json":
		return true
	default:
		return false
	}
}
