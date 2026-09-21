// Package spotify ingests Spotify Extended Streaming History exports.
//
// The ingestion boundary deliberately delivers plays to a consumer as they
// are decoded. It does not retain the export or return a collection of plays.
package spotify

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

const (
	// Diagnostic codes are stable identifiers for consumers and reports.
	CodeCorruptJSON      = "corrupt_json"
	CodeMalformedRecord  = "malformed_record"
	CodeMissingField     = "missing_required_field"
	CodePodcast          = "excluded_podcast"
	CodeLocalOrOffline   = "excluded_local_or_offline"
	CodeInputUnavailable = "input_unavailable"
)

// Input is one JSON export supplied to Ingest. Name is an input identity used
// in diagnostics and should normally be the source file name.
type Input struct {
	Name   string
	Reader io.Reader
}

// Play is the normalized representation consumed by later comparison and
// import stages.
type Play struct {
	TrackName       string    `json:"track_name"`
	ArtistName      string    `json:"artist_name"`
	AlbumName       string    `json:"album_name"`
	Timestamp       time.Time `json:"timestamp"`
	Milliseconds    int64     `json:"milliseconds_played"`
	Platform        string    `json:"platform"`
	SpotifyTrackURI string    `json:"spotify_track_uri"`
	SpotifyTrackID  string    `json:"spotify_track_id"`
}

// Warning is a structured, non-fatal ingestion diagnostic. Record is
// one-based within its input; it is zero for an input-level diagnostic.
type Warning struct {
	Input    string `json:"input"`
	Record   int    `json:"record,omitempty"`
	Code     string `json:"code"`
	Reason   string `json:"reason"`
	Field    string `json:"field,omitempty"`
	Severity string `json:"severity"`
}

// Summary contains counts only; plays are never accumulated by the parser.
type Summary struct {
	Inputs               int `json:"inputs"`
	Records              int `json:"records"`
	Emitted              int `json:"emitted"`
	Warnings             int `json:"warnings"`
	ExcludedPodcasts     int `json:"excluded_podcasts"`
	ExcludedLocalOffline int `json:"excluded_local_offline"`
}

// Consumer receives one normalized play. Returning an error stops ingestion.
type Consumer func(Play) error

// WarningHandler receives one structured diagnostic. A nil handler discards
// diagnostics.
type WarningHandler func(Warning)

// Ingest streams one or more Extended Streaming History JSON arrays to
// consumer. A corrupt input produces one warning and processing continues with
// later inputs. Consumer and context errors are returned because they
// indicate that the caller, rather than the input, stopped the pipeline.
func Ingest(ctx context.Context, inputs []Input, consumer Consumer, warningHandler WarningHandler) (Summary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if consumer == nil {
		return Summary{}, errors.New("spotify ingestion consumer is nil")
	}

	var summary Summary
	summary.Inputs = len(inputs)
	for index, input := range inputs {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		if strings.TrimSpace(input.Name) == "" {
			input.Name = fmt.Sprintf("input-%d", index+1)
		}
		if input.Reader == nil {
			emitWarning(&summary, warningHandler, Warning{
				Input:    input.Name,
				Code:     CodeInputUnavailable,
				Reason:   "input reader is nil",
				Severity: "warning",
			})
			continue
		}
		if err := ingestInput(ctx, input, consumer, warningHandler, &summary); err != nil {
			return summary, err
		}
	}
	return summary, nil
}

// IngestFiles opens and streams the named JSON export files in order.
func IngestFiles(ctx context.Context, paths []string, consumer Consumer, warningHandler WarningHandler) (Summary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if consumer == nil {
		return Summary{}, errors.New("spotify ingestion consumer is nil")
	}

	summary := Summary{Inputs: len(paths)}
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return summary, err
		}
		file, err := os.Open(path)
		if err != nil {
			// Keep the file helper's behavior consistent with corrupt input:
			// report an unavailable file and continue opening later files.
			emitWarning(&summary, warningHandler, Warning{
				Input:    path,
				Code:     CodeInputUnavailable,
				Reason:   err.Error(),
				Severity: "warning",
			})
			continue
		}

		fileSummary, ingestErr := Ingest(ctx, []Input{{Name: path, Reader: file}}, consumer, warningHandler)
		closeErr := file.Close()
		summary.Records += fileSummary.Records
		summary.Emitted += fileSummary.Emitted
		summary.Warnings += fileSummary.Warnings
		summary.ExcludedPodcasts += fileSummary.ExcludedPodcasts
		summary.ExcludedLocalOffline += fileSummary.ExcludedLocalOffline
		if ingestErr != nil {
			return summary, ingestErr
		}
		if closeErr != nil {
			return summary, fmt.Errorf("close Spotify input %q: %w", path, closeErr)
		}
	}
	return summary, nil
}

func ingestInput(ctx context.Context, input Input, consumer Consumer, warningHandler WarningHandler, summary *Summary) error {
	decoder := json.NewDecoder(input.Reader)
	token, err := decoder.Token()
	if err != nil {
		emitWarning(summary, warningHandler, corruptWarning(input.Name, err))
		return nil
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '[' {
		emitWarning(summary, warningHandler, Warning{
			Input:    input.Name,
			Code:     CodeCorruptJSON,
			Reason:   "top-level JSON value must be an array",
			Severity: "warning",
		})
		return nil
	}

	recordNumber := 0
	for decoder.More() {
		if err := ctx.Err(); err != nil {
			return err
		}
		recordNumber++
		var raw json.RawMessage
		if err := decoder.Decode(&raw); err != nil {
			emitWarning(summary, warningHandler, corruptWarningAt(input.Name, recordNumber, err))
			return nil
		}
		summary.Records++
		if err := processRecord(ctx, input.Name, recordNumber, raw, consumer, warningHandler, summary); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		emitWarning(summary, warningHandler, corruptWarning(input.Name, err))
		return nil
	}

	// A valid array followed by another value or trailing non-JSON bytes is a
	// corrupt input, and receives exactly one input-level diagnostic.
	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			err = errors.New("unexpected JSON value after top-level array")
		}
		emitWarning(summary, warningHandler, corruptWarning(input.Name, err))
	}
	return nil
}

type rawRecord struct {
	Timestamp         *string `json:"ts"`
	Platform          *string `json:"platform"`
	Milliseconds      *int64  `json:"ms_played"`
	TrackName         *string `json:"master_metadata_track_name"`
	ArtistName        *string `json:"master_metadata_album_artist_name"`
	AlbumName         *string `json:"master_metadata_album_album_name"`
	SpotifyTrackURI   *string `json:"spotify_track_uri"`
	SpotifyTrackID    *string `json:"spotify_track_id"`
	EpisodeName       *string `json:"episode_name"`
	EpisodeShowName   *string `json:"episode_show_name"`
	SpotifyEpisodeURI *string `json:"spotify_episode_uri"`
	Offline           *bool   `json:"offline"`
}

func processRecord(ctx context.Context, input string, number int, raw json.RawMessage, consumer Consumer, warningHandler WarningHandler, summary *Summary) error {
	if len(raw) == 0 || raw[0] != '{' {
		emitWarning(summary, warningHandler, Warning{
			Input:    input,
			Record:   number,
			Code:     CodeMalformedRecord,
			Reason:   "record must be a JSON object",
			Severity: "warning",
		})
		return nil
	}
	var record rawRecord
	if err := json.Unmarshal(raw, &record); err != nil {
		emitWarning(summary, warningHandler, Warning{
			Input:    input,
			Record:   number,
			Code:     CodeMalformedRecord,
			Reason:   "record is not a valid Spotify object: " + err.Error(),
			Severity: "warning",
		})
		return nil
	}

	if isPodcast(record) {
		summary.ExcludedPodcasts++
		emitWarning(summary, warningHandler, Warning{
			Input:    input,
			Record:   number,
			Code:     CodePodcast,
			Reason:   "record contains Spotify episode metadata",
			Severity: "warning",
		})
		return nil
	}
	if record.Offline != nil && *record.Offline {
		summary.ExcludedLocalOffline++
		emitWarning(summary, warningHandler, Warning{
			Input:    input,
			Record:   number,
			Code:     CodeLocalOrOffline,
			Reason:   "record is marked offline",
			Severity: "warning",
		})
		return nil
	}
	uri := ""
	if record.SpotifyTrackURI != nil {
		uri = strings.TrimSpace(*record.SpotifyTrackURI)
	}
	if !strings.HasPrefix(uri, "spotify:track:") || strings.TrimPrefix(uri, "spotify:track:") == "" {
		summary.ExcludedLocalOffline++
		emitWarning(summary, warningHandler, Warning{
			Input:    input,
			Record:   number,
			Code:     CodeLocalOrOffline,
			Reason:   "record has no Spotify track URI",
			Severity: "warning",
		})
		return nil
	}

	missing := func(field string) {
		emitWarning(summary, warningHandler, Warning{
			Input:    input,
			Record:   number,
			Code:     CodeMissingField,
			Reason:   "required music metadata field is missing or empty",
			Field:    field,
			Severity: "warning",
		})
	}
	requiredStrings := []struct {
		name  string
		value *string
	}{
		{"ts", record.Timestamp},
		{"platform", record.Platform},
		{"master_metadata_track_name", record.TrackName},
		{"master_metadata_album_artist_name", record.ArtistName},
		{"master_metadata_album_album_name", record.AlbumName},
	}
	for _, field := range requiredStrings {
		if field.value == nil || strings.TrimSpace(*field.value) == "" {
			missing(field.name)
			return nil
		}
	}
	if record.Milliseconds == nil {
		missing("ms_played")
		return nil
	}
	if *record.Milliseconds < 0 {
		emitWarning(summary, warningHandler, Warning{
			Input:    input,
			Record:   number,
			Code:     CodeMalformedRecord,
			Reason:   "ms_played must not be negative",
			Field:    "ms_played",
			Severity: "warning",
		})
		return nil
	}
	playedAt, err := time.Parse(time.RFC3339Nano, strings.TrimSpace(*record.Timestamp))
	if err != nil {
		emitWarning(summary, warningHandler, Warning{
			Input:    input,
			Record:   number,
			Code:     CodeMalformedRecord,
			Reason:   "ts is not an RFC3339 timestamp: " + err.Error(),
			Field:    "ts",
			Severity: "warning",
		})
		return nil
	}

	trackID := strings.TrimSpace(pointerValue(record.SpotifyTrackID))
	if trackID == "" {
		trackID = spotifyTrackID(uri)
	}
	play := Play{
		TrackName:       strings.TrimSpace(*record.TrackName),
		ArtistName:      strings.TrimSpace(*record.ArtistName),
		AlbumName:       strings.TrimSpace(*record.AlbumName),
		Timestamp:       playedAt,
		Milliseconds:    *record.Milliseconds,
		Platform:        strings.TrimSpace(*record.Platform),
		SpotifyTrackURI: uri,
		SpotifyTrackID:  trackID,
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := consumer(play); err != nil {
		return fmt.Errorf("consume Spotify play from %q record %d: %w", input, number, err)
	}
	summary.Emitted++
	return nil
}

func isPodcast(record rawRecord) bool {
	return nonEmpty(record.EpisodeName) ||
		nonEmpty(record.EpisodeShowName) ||
		nonEmpty(record.SpotifyEpisodeURI) ||
		strings.HasPrefix(strings.TrimSpace(pointerValue(record.SpotifyTrackURI)), "spotify:episode:")
}

func nonEmpty(value *string) bool {
	return value != nil && strings.TrimSpace(*value) != ""
}

func pointerValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func spotifyTrackID(uri string) string {
	const prefix = "spotify:track:"
	if strings.HasPrefix(uri, prefix) {
		return strings.TrimPrefix(uri, prefix)
	}
	return ""
}

func corruptWarning(input string, err error) Warning {
	return corruptWarningAt(input, 0, err)
}

func corruptWarningAt(input string, record int, err error) Warning {
	return Warning{
		Input:    input,
		Record:   record,
		Code:     CodeCorruptJSON,
		Reason:   "input is not valid JSON: " + err.Error(),
		Severity: "warning",
	}
}

func emitWarning(summary *Summary, handler WarningHandler, warning Warning) {
	summary.Warnings++
	if handler != nil {
		handler(warning)
	}
}
