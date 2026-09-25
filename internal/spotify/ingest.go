// Package spotify ingests Spotify Extended Streaming History exports.
//
// The ingestion boundary deliberately delivers plays to a consumer as they
// are decoded. It does not retain the export or return a collection of plays.
package spotify

import (
	"archive/zip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"strings"
	"time"
)

const (
	// Diagnostic codes are stable identifiers for consumers and reports.
	CodeCorruptJSON      = "corrupt_json"
	CodeMalformedRecord  = "malformed_record"
	CodeMissingField     = "missing_required_field"
	CodePodcast          = "excluded_podcast"
	CodeLocal            = "excluded_local"
	CodeInputUnavailable = "input_unavailable"
	CodeArchivePath      = "unsafe_archive_path"
	CodeArchiveEntry     = "unreadable_archive_entry"

	// DefaultMaxArchiveUncompressedBytes bounds the total declared
	// uncompressed content in one archive when no explicit limit is supplied.
	DefaultMaxArchiveUncompressedBytes uint64 = 512 << 20
)

// Input is one JSON export supplied to Ingest. Name is an input identity used
// in diagnostics and should normally be the source file name. Names ending
// in .zip are treated as Spotify ZIP exports.
type Input struct {
	Name   string
	Reader io.Reader
}

// IngestOptions controls resource limits for archive ingestion.
type IngestOptions struct {
	// MaxArchiveUncompressedBytes is the maximum cumulative uncompressed size
	// of entries in one archive. Zero selects the default limit.
	MaxArchiveUncompressedBytes uint64
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
	Inputs           int `json:"inputs"`
	Records          int `json:"records"`
	Emitted          int `json:"emitted"`
	Warnings         int `json:"warnings"`
	ExcludedPodcasts int `json:"excluded_podcasts"`
	ExcludedLocal    int `json:"excluded_local"`
}

// Consumer receives one normalized play. Returning an error stops ingestion.
type Consumer func(Play) error

// WarningHandler receives one structured diagnostic. A nil handler discards
// diagnostics.
type WarningHandler func(Warning)

var errArchiveLimitExceeded = errors.New("archive uncompressed byte limit exceeded")

// Ingest streams one or more Extended Streaming History JSON arrays to
// consumer. A corrupt input produces one warning and processing continues with
// later inputs. Consumer and context errors are returned because they
// indicate that the caller, rather than the input, stopped the pipeline.
func Ingest(ctx context.Context, inputs []Input, consumer Consumer, warningHandler WarningHandler) (Summary, error) {
	return IngestWithOptions(ctx, inputs, consumer, warningHandler, IngestOptions{})
}

// IngestWithOptions streams raw JSON inputs and ZIP archives to consumer.
func IngestWithOptions(ctx context.Context, inputs []Input, consumer Consumer, warningHandler WarningHandler, options IngestOptions) (Summary, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if consumer == nil {
		return Summary{}, errors.New("spotify ingestion consumer is nil")
	}
	options = normalizeIngestOptions(options)

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
		var err error
		if isZIPInput(input.Name) {
			err = ingestArchive(ctx, input, consumer, warningHandler, &summary, options)
		} else {
			err = ingestInput(ctx, input, consumer, warningHandler, &summary)
		}
		if err != nil {
			return summary, err
		}
	}
	return summary, nil
}

// IngestFiles opens and streams the named JSON export files in order.
func IngestFiles(ctx context.Context, paths []string, consumer Consumer, warningHandler WarningHandler) (Summary, error) {
	return IngestFilesWithOptions(ctx, paths, consumer, warningHandler, IngestOptions{})
}

// IngestFilesWithOptions opens and streams named JSON or ZIP export files in
// order.
func IngestFilesWithOptions(ctx context.Context, paths []string, consumer Consumer, warningHandler WarningHandler, options IngestOptions) (Summary, error) {
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

		fileSummary, ingestErr := IngestWithOptions(ctx, []Input{{Name: path, Reader: file}}, consumer, warningHandler, options)
		closeErr := file.Close()
		summary.Records += fileSummary.Records
		summary.Emitted += fileSummary.Emitted
		summary.Warnings += fileSummary.Warnings
		summary.ExcludedPodcasts += fileSummary.ExcludedPodcasts
		summary.ExcludedLocal += fileSummary.ExcludedLocal
		if ingestErr != nil {
			return summary, ingestErr
		}
		if closeErr != nil {
			return summary, fmt.Errorf("close Spotify input %q: %w", path, closeErr)
		}
	}
	return summary, nil
}

func normalizeIngestOptions(options IngestOptions) IngestOptions {
	if options.MaxArchiveUncompressedBytes == 0 {
		options.MaxArchiveUncompressedBytes = DefaultMaxArchiveUncompressedBytes
	}
	return options
}

func isZIPInput(name string) bool {
	return strings.EqualFold(path.Ext(strings.TrimSpace(name)), ".zip")
}

func ingestArchive(ctx context.Context, input Input, consumer Consumer, warningHandler WarningHandler, summary *Summary, options IngestOptions) error {
	reader, cleanup, err := openArchiveReader(input.Reader)
	if err != nil {
		return fmt.Errorf("open Spotify archive %q: %w", input.Name, err)
	}
	defer cleanup()

	declaredBytes := uint64(0)
	for _, entry := range reader.File {
		if err := ctx.Err(); err != nil {
			return err
		}
		unsafeName := unsafeArchiveName(entry.Name)
		if unsafeName {
			emitWarning(summary, warningHandler, Warning{
				Input:    archiveEntryInput(input.Name, entry.Name),
				Code:     CodeArchivePath,
				Reason:   "archive entry name is absolute or contains parent-directory traversal",
				Severity: "warning",
			})
		}
		if entry.UncompressedSize64 > options.MaxArchiveUncompressedBytes-declaredBytes {
			return fmt.Errorf(
				"Spotify archive %q exceeds maximum cumulative uncompressed size of %d bytes",
				input.Name, options.MaxArchiveUncompressedBytes,
			)
		}
		previousDeclaredBytes := declaredBytes
		declaredBytes += entry.UncompressedSize64
		if unsafeName || entry.FileInfo().IsDir() || !strings.EqualFold(path.Ext(entry.Name), ".json") {
			continue
		}

		entryReader, err := entry.Open()
		if err != nil {
			emitWarning(summary, warningHandler, Warning{
				Input:    archiveEntryInput(input.Name, entry.Name),
				Code:     CodeArchiveEntry,
				Reason:   "archive entry could not be opened: " + err.Error(),
				Severity: "warning",
			})
			continue
		}
		entryInput := Input{
			Name: archiveEntryInput(input.Name, entry.Name),
			Reader: &archiveLimitReader{
				Reader:    entryReader,
				Remaining: options.MaxArchiveUncompressedBytes - previousDeclaredBytes,
			},
		}
		ingestErr := ingestInput(ctx, entryInput, consumer, warningHandler, summary)
		closeErr := entryReader.Close()
		if ingestErr != nil {
			return ingestErr
		}
		if closeErr != nil {
			emitWarning(summary, warningHandler, Warning{
				Input:    entryInput.Name,
				Code:     CodeArchiveEntry,
				Reason:   "archive entry could not be read: " + closeErr.Error(),
				Severity: "warning",
			})
		}
	}
	return nil
}

type archiveLimitReader struct {
	io.Reader
	Remaining uint64
}

func (reader *archiveLimitReader) Read(p []byte) (int, error) {
	if reader.Remaining == 0 {
		var probe [1]byte
		n, err := reader.Reader.Read(probe[:])
		if n > 0 {
			return 0, errArchiveLimitExceeded
		}
		return 0, err
	}
	readLength := len(p)
	if uint64(readLength) > reader.Remaining+1 {
		readLength = int(reader.Remaining + 1)
	}
	n, err := reader.Reader.Read(p[:readLength])
	if uint64(n) > reader.Remaining {
		return int(reader.Remaining), errArchiveLimitExceeded
	}
	reader.Remaining -= uint64(n)
	return n, err
}

func archiveEntryInput(archiveName, entryName string) string {
	return archiveName + "::" + entryName
}

func unsafeArchiveName(name string) bool {
	normalized := strings.ReplaceAll(name, `\`, "/")
	if path.IsAbs(normalized) || strings.HasPrefix(normalized, "/") {
		return true
	}
	if len(normalized) >= 2 && normalized[1] == ':' {
		return true
	}
	for _, segment := range strings.Split(normalized, "/") {
		if segment == ".." {
			return true
		}
	}
	return false
}

func openArchiveReader(input io.Reader) (*zip.Reader, func(), error) {
	if readerAt, ok := input.(io.ReaderAt); ok {
		if seeker, ok := input.(io.Seeker); ok {
			current, err := seeker.Seek(0, io.SeekCurrent)
			if err != nil {
				return nil, func() {}, err
			}
			size, err := seeker.Seek(0, io.SeekEnd)
			if err != nil {
				return nil, func() {}, err
			}
			if _, err := seeker.Seek(current, io.SeekStart); err != nil {
				return nil, func() {}, err
			}
			if current < 0 || size < current {
				return nil, func() {}, errors.New("archive reader position is outside its bounds")
			}
			reader, err := zip.NewReader(io.NewSectionReader(readerAt, current, size-current), size-current)
			return reader, func() {}, err
		}
	}

	file, err := os.CreateTemp("", "rescrobble-spotify-archive-*")
	if err != nil {
		return nil, func() {}, fmt.Errorf("stage archive: %w", err)
	}
	cleanup := func() {
		_ = file.Close()
		_ = os.Remove(file.Name())
	}
	if _, err := io.Copy(file, input); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("stage archive: %w", err)
	}
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("rewind staged archive: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		cleanup()
		return nil, func() {}, fmt.Errorf("stat staged archive: %w", err)
	}
	reader, err := zip.NewReader(file, info.Size())
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return reader, cleanup, nil
}

func ingestInput(ctx context.Context, input Input, consumer Consumer, warningHandler WarningHandler, summary *Summary) error {
	decoder := json.NewDecoder(input.Reader)
	token, err := decoder.Token()
	if err != nil {
		if errors.Is(err, errArchiveLimitExceeded) {
			return err
		}
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
			if errors.Is(err, errArchiveLimitExceeded) {
				return err
			}
			emitWarning(summary, warningHandler, corruptWarningAt(input.Name, recordNumber, err))
			return nil
		}
		summary.Records++
		if err := processRecord(ctx, input.Name, recordNumber, raw, consumer, warningHandler, summary); err != nil {
			return err
		}
	}
	if _, err := decoder.Token(); err != nil {
		if errors.Is(err, errArchiveLimitExceeded) {
			return err
		}
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
	uri := ""
	if record.SpotifyTrackURI != nil {
		uri = strings.TrimSpace(*record.SpotifyTrackURI)
	}
	const spotifyTrackPrefix = "spotify:track:"
	if !strings.HasPrefix(uri, spotifyTrackPrefix) || strings.TrimSpace(strings.TrimPrefix(uri, spotifyTrackPrefix)) == "" {
		summary.ExcludedLocal++
		emitWarning(summary, warningHandler, Warning{
			Input:    input,
			Record:   number,
			Code:     CodeLocal,
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
		AlbumName:       strings.TrimSpace(pointerValue(record.AlbumName)),
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
