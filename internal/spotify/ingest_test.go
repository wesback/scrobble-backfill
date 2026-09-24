package spotify

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestIngestNormalizesMusicRecordsAndReportsSkippedInputs(t *testing.T) {
	valid := recordJSON("Track", "Artist", "Album", "spotify:track:abc")
	malformed := `{"ts":"2024-01-01T00:00:00Z","platform":"web","ms_played":"bad"}`
	missing := recordJSON("Track", "", "Album", "spotify:track:missing-artist")
	podcast := recordJSON("Episode", "Show", "Podcast", "spotify:track:episode-data")
	podcast = strings.Replace(podcast, `"spotify_track_uri":"spotify:track:episode-data"`, `"spotify_track_uri":"spotify:episode:episode-data","episode_name":"Episode"`, 1)
	offline := strings.Replace(recordJSON("Offline", "Artist", "Album", "spotify:track:offline"), `"offline":false`, `"offline":true`, 1)
	local := strings.Replace(recordJSON("Local", "Artist", "Album", "spotify:local:file"), `"spotify_track_uri":"spotify:local:file"`, `"spotify_track_uri":""`, 1)
	validLater := recordJSON("Later", "Artist", "Album", "spotify:track:later")
	inputs := []Input{
		{Name: "history-valid.json", Reader: strings.NewReader("[" + strings.Join([]string{valid, malformed, missing, podcast, offline, local}, ",") + "]")},
		{Name: "history-corrupt.json", Reader: strings.NewReader("[{\"ts\":")},
		{Name: "history-later.json", Reader: strings.NewReader("[" + validLater + "]")},
	}

	var plays []Play
	var warnings []Warning
	summary, err := Ingest(context.Background(), inputs, func(play Play) error {
		plays = append(plays, play)
		return nil
	}, func(warning Warning) {
		warnings = append(warnings, warning)
	})
	if err != nil {
		t.Fatalf("ingest: %v", err)
	}
	if len(plays) != 2 || summary.Emitted != 2 {
		t.Fatalf("emitted plays = %d (summary %d), want 2", len(plays), summary.Emitted)
	}
	if got := plays[0]; got.TrackName != "Track" || got.ArtistName != "Artist" ||
		got.AlbumName != "Album" || got.Milliseconds != 180000 ||
		got.Platform != "web" || got.SpotifyTrackURI != "spotify:track:abc" ||
		got.SpotifyTrackID != "abc" || !got.Timestamp.Equal(time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("normalized play = %#v", got)
	}
	if summary.ExcludedPodcasts != 1 || summary.ExcludedLocalOffline != 2 {
		t.Fatalf("exclusion counts = podcast %d, local/offline %d; want 1 and 2", summary.ExcludedPodcasts, summary.ExcludedLocalOffline)
	}
	if summary.Warnings != 6 || len(warnings) != 6 {
		t.Fatalf("warnings = summary %d, collected %d; want 6", summary.Warnings, len(warnings))
	}
	counts := map[string]int{}
	for _, warning := range warnings {
		if warning.Input == "" || warning.Reason == "" || warning.Severity != "warning" {
			t.Fatalf("warning is not structured: %#v", warning)
		}
		counts[warning.Code]++
	}
	wantCodes := map[string]int{
		CodeMalformedRecord: 1,
		CodeMissingField:    1,
		CodePodcast:         1,
		CodeLocalOrOffline:  2,
		CodeCorruptJSON:     1,
	}
	for code, want := range wantCodes {
		if counts[code] != want {
			t.Fatalf("warning code %q count = %d, want %d", code, counts[code], want)
		}
	}
}

func TestIngestTreatsAlbumMetadataAsOptional(t *testing.T) {
	nullAlbum := strings.Replace(
		recordJSON("Track", "Artist", "Album", "spotify:track:null-album"),
		`"master_metadata_album_album_name":"Album"`,
		`"master_metadata_album_album_name":null`,
		1,
	)
	tests := []struct {
		name   string
		record string
	}{
		{name: "null album", record: nullAlbum},
		{name: "empty album", record: recordJSON("Track", "Artist", "", "spotify:track:empty-album")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var plays []Play
			var warnings []Warning
			summary, err := Ingest(context.Background(), []Input{{
				Name:   "history.json",
				Reader: strings.NewReader("[" + test.record + "]"),
			}}, func(play Play) error {
				plays = append(plays, play)
				return nil
			}, func(warning Warning) {
				warnings = append(warnings, warning)
			})
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if len(plays) != 1 || summary.Emitted != 1 || plays[0].AlbumName != "" {
				t.Fatalf("plays = %#v, summary = %#v; want one play with empty album", plays, summary)
			}
			for _, warning := range warnings {
				if warning.Code == CodeMissingField && warning.Field == "master_metadata_album_album_name" {
					t.Fatalf("album produced a missing-field warning: %#v", warning)
				}
			}
		})
	}
}

func TestIngestStillRequiresArtistAndTrackMetadata(t *testing.T) {
	tests := []struct {
		name   string
		record string
		field  string
	}{
		{
			name:   "artist",
			record: recordJSON("Track", "", "Album", "spotify:track:missing-artist"),
			field:  "master_metadata_album_artist_name",
		},
		{
			name:   "track",
			record: recordJSON("", "Artist", "Album", "spotify:track:missing-track"),
			field:  "master_metadata_track_name",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var plays []Play
			var warnings []Warning
			_, err := Ingest(context.Background(), []Input{{
				Name:   "history.json",
				Reader: strings.NewReader("[" + test.record + "]"),
			}}, func(play Play) error {
				plays = append(plays, play)
				return nil
			}, func(warning Warning) {
				warnings = append(warnings, warning)
			})
			if err != nil {
				t.Fatalf("ingest: %v", err)
			}
			if len(plays) != 0 {
				t.Fatalf("plays = %#v; want no plays", plays)
			}
			if len(warnings) != 1 || warnings[0].Code != CodeMissingField ||
				warnings[0].Field != test.field || warnings[0].Severity != "warning" {
				t.Fatalf("warnings = %#v; want one missing-field warning for %q", warnings, test.field)
			}
		})
	}
}

func TestIngestStreamsGenerated100000Records(t *testing.T) {
	const total = 100_000
	var input bytes.Buffer
	input.WriteByte('[')
	encoder := json.NewEncoder(&input)
	valid := 0
	for i := 0; i < total; i++ {
		var value any
		switch i % 1000 {
		case 0:
			value = map[string]any{
				"ts": "2024-01-01T00:00:00Z", "platform": "web", "ms_played": 1,
				"master_metadata_track_name": "episode", "master_metadata_album_artist_name": "show",
				"master_metadata_album_album_name": "podcast", "spotify_episode_uri": "spotify:episode:e",
			}
		case 1:
			value = map[string]any{
				"ts": "2024-01-01T00:00:00Z", "platform": "web", "ms_played": 1,
				"master_metadata_track_name": "offline", "master_metadata_album_artist_name": "artist",
				"master_metadata_album_album_name": "album", "spotify_track_uri": "spotify:track:o", "offline": true,
			}
		case 2:
			value = map[string]any{
				"ts": "2024-01-01T00:00:00Z", "platform": "web", "ms_played": 1,
				"master_metadata_track_name": "local", "master_metadata_album_artist_name": "artist",
				"master_metadata_album_album_name": "album",
			}
		case 3:
			value = 42
		default:
			valid++
			value = map[string]any{
				"ts": "2024-01-01T00:00:00Z", "platform": "web", "ms_played": i,
				"master_metadata_track_name": "track", "master_metadata_album_artist_name": "artist",
				"master_metadata_album_album_name": "album", "spotify_track_uri": "spotify:track:valid",
			}
		}
		if i > 0 {
			// Encoder writes one complete JSON value at a time; commas are
			// inserted between values to retain the streaming input shape.
			// The preceding value's newline is harmless JSON whitespace.
			input.WriteByte(',')
		}
		if err := encoder.Encode(value); err != nil {
			t.Fatalf("encode generated record %d: %v", i, err)
		}
	}
	input.WriteByte(']')

	delivered := 0
	var warningCodes map[string]int = make(map[string]int)
	summary, err := Ingest(context.Background(), []Input{{Name: "generated-100k.json", Reader: &input}}, func(Play) error {
		delivered++
		return nil
	}, func(warning Warning) {
		warningCodes[warning.Code]++
	})
	if err != nil {
		t.Fatalf("ingest generated input: %v", err)
	}
	if delivered != valid || summary.Emitted != valid {
		t.Fatalf("delivered = %d (summary %d), want %d valid records", delivered, summary.Emitted, valid)
	}
	if summary.Records != total || summary.ExcludedPodcasts != 100 || summary.ExcludedLocalOffline != 200 {
		t.Fatalf("summary = %#v, want %d records, 100 podcasts, 200 local/offline", summary, total)
	}
	if warningCodes[CodeMalformedRecord] != 100 || summary.Warnings != 400 {
		t.Fatalf("warning codes = %#v, summary warnings = %d; want 100 malformed and 400 total", warningCodes, summary.Warnings)
	}
}

func TestIngestFilesContinuesAfterCorruptFile(t *testing.T) {
	dir := t.TempDir()
	corruptPath := filepath.Join(dir, "corrupt.json")
	validPath := filepath.Join(dir, "valid.json")
	if err := os.WriteFile(corruptPath, []byte(`[{"ts":`), 0o600); err != nil {
		t.Fatalf("write corrupt input: %v", err)
	}
	if err := os.WriteFile(validPath, []byte("["+recordJSON("Track", "Artist", "Album", "spotify:track:valid")+"]"), 0o600); err != nil {
		t.Fatalf("write valid input: %v", err)
	}

	var plays []Play
	var warnings []Warning
	summary, err := IngestFiles(context.Background(), []string{corruptPath, validPath}, func(play Play) error {
		plays = append(plays, play)
		return nil
	}, func(warning Warning) {
		warnings = append(warnings, warning)
	})
	if err != nil {
		t.Fatalf("ingest files: %v", err)
	}
	if len(plays) != 1 || summary.Emitted != 1 {
		t.Fatalf("emitted plays = %d (summary %d), want 1", len(plays), summary.Emitted)
	}
	if len(warnings) != 1 || warnings[0].Code != CodeCorruptJSON || warnings[0].Input != corruptPath {
		t.Fatalf("warnings = %#v, want one corrupt warning for %q", warnings, corruptPath)
	}
}

func TestIngestStreamsMultipleZIPInputsAndContinuesAfterCorruptEntry(t *testing.T) {
	first := zipData(t, []zipEntry{
		{Name: "StreamingHistory_music_0.json", Data: "[" + recordJSON("First", "Artist", "Album", "spotify:track:first") + "]"},
		{Name: "corrupt.json", Data: `[{"ts":`},
	})
	second := zipData(t, []zipEntry{
		{Name: "StreamingHistory_music_1.json", Data: "[" + recordJSON("Second", "Artist", "Album", "spotify:track:second") + "]"},
	})

	var plays []Play
	var warnings []Warning
	summary, err := Ingest(context.Background(), []Input{
		{Name: "spotify-first.zip", Reader: bytes.NewReader(first)},
		{Name: "spotify-second.zip", Reader: bytes.NewReader(second)},
		{Name: "separate.json", Reader: strings.NewReader("[" + recordJSON("Raw", "Artist", "Album", "spotify:track:raw") + "]")},
	}, func(play Play) error {
		plays = append(plays, play)
		return nil
	}, func(warning Warning) {
		warnings = append(warnings, warning)
	})
	if err != nil {
		t.Fatalf("ingest ZIP inputs: %v", err)
	}
	if summary.Inputs != 3 || summary.Emitted != 3 || len(plays) != 3 {
		t.Fatalf("summary = %#v, plays = %#v; want three emitted plays", summary, plays)
	}
	if len(warnings) != 1 || warnings[0].Code != CodeCorruptJSON ||
		!strings.Contains(warnings[0].Input, "spotify-first.zip::corrupt.json") {
		t.Fatalf("warnings = %#v, want one corrupt ZIP-entry warning", warnings)
	}
}

func TestIngestRejectsUnsafeZIPEntryNamesWithoutOpeningThem(t *testing.T) {
	archive := zipData(t, []zipEntry{
		{Name: "safe/history.json", Data: "[" + recordJSON("Safe", "Artist", "Album", "spotify:track:safe") + "]"},
		{Name: "../escape.json", Data: "[" + recordJSON("Escape", "Artist", "Album", "spotify:track:escape") + "]"},
		{Name: "/absolute.json", Data: "[" + recordJSON("Absolute", "Artist", "Album", "spotify:track:absolute") + "]"},
		{Name: `nested\..\windows.json`, Data: "[" + recordJSON("Windows", "Artist", "Album", "spotify:track:windows") + "]"},
	})

	var plays []Play
	var warnings []Warning
	summary, err := Ingest(context.Background(), []Input{{Name: "unsafe.zip", Reader: bytes.NewReader(archive)}}, func(play Play) error {
		plays = append(plays, play)
		return nil
	}, func(warning Warning) {
		warnings = append(warnings, warning)
	})
	if err != nil {
		t.Fatalf("ingest unsafe ZIP: %v", err)
	}
	if len(plays) != 1 || plays[0].TrackName != "Safe" || summary.Emitted != 1 {
		t.Fatalf("plays = %#v, summary = %#v; want only safe entry", plays, summary)
	}
	if len(warnings) != 3 {
		t.Fatalf("warnings = %#v; want three unsafe-path warnings", warnings)
	}
	for _, warning := range warnings {
		if warning.Code != CodeArchivePath || warning.Severity != "warning" {
			t.Fatalf("warning = %#v; want structured unsafe-path warning", warning)
		}
	}
}

func TestIngestZIPEnforcesConfiguredCumulativeUncompressedLimit(t *testing.T) {
	entryJSON := "[" + strings.Repeat(" ", 600) + "]"
	archive := zipData(t, []zipEntry{
		{Name: "history-0.json", Data: entryJSON},
		{Name: "history-1.json", Data: entryJSON},
	})

	var plays []Play
	_, err := IngestWithOptions(context.Background(), []Input{{Name: "too-large.zip", Reader: bytes.NewReader(archive)}}, func(play Play) error {
		plays = append(plays, play)
		return nil
	}, nil, IngestOptions{MaxArchiveUncompressedBytes: 1024})
	if err == nil || !strings.Contains(err.Error(), "maximum cumulative uncompressed size of 1024 bytes") {
		t.Fatalf("error = %v; want clear archive size-limit error", err)
	}
	if len(plays) != 0 {
		t.Fatalf("plays = %#v; want no content emitted after limit rejection", plays)
	}
}

func TestIngestZIPFromNonzeroReaderOffset(t *testing.T) {
	archive := zipData(t, []zipEntry{
		{Name: "history.json", Data: "[" + recordJSON("Offset", "Artist", "Album", "spotify:track:offset") + "]"},
	})
	input := append([]byte("archive prefix"), archive...)
	reader := bytes.NewReader(input)
	if _, err := reader.Seek(int64(len("archive prefix")), io.SeekStart); err != nil {
		t.Fatalf("seek to archive: %v", err)
	}

	var plays []Play
	summary, err := Ingest(context.Background(), []Input{{Name: "offset.zip", Reader: reader}}, func(play Play) error {
		plays = append(plays, play)
		return nil
	}, nil)
	if err != nil {
		t.Fatalf("ingest offset ZIP: %v", err)
	}
	if summary.Emitted != 1 || len(plays) != 1 || plays[0].TrackName != "Offset" {
		t.Fatalf("summary = %#v, plays = %#v; want one offset archive play", summary, plays)
	}
}

type zipEntry struct {
	Name string
	Data string
}

func zipData(t *testing.T, entries []zipEntry) []byte {
	t.Helper()
	var output bytes.Buffer
	writer := zip.NewWriter(&output)
	for _, entry := range entries {
		file, err := writer.Create(entry.Name)
		if err != nil {
			t.Fatalf("create ZIP entry %q: %v", entry.Name, err)
		}
		if _, err := file.Write([]byte(entry.Data)); err != nil {
			t.Fatalf("write ZIP entry %q: %v", entry.Name, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close ZIP: %v", err)
	}
	return output.Bytes()
}

func recordJSON(track, artist, album, uri string) string {
	value := map[string]any{
		"ts": "2024-01-01T00:00:00Z", "platform": "web", "ms_played": 180000,
		"master_metadata_track_name": track, "master_metadata_album_artist_name": artist,
		"master_metadata_album_album_name": album, "spotify_track_uri": uri, "offline": false,
	}
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return string(data)
}
