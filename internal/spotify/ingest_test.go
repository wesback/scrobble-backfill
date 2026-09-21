package spotify

import (
	"bytes"
	"context"
	"encoding/json"
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
