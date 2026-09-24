package cli

import (
	"bytes"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wesback/scrobble-backfill/internal/config"
	"github.com/wesback/scrobble-backfill/internal/credentials"
	"github.com/wesback/scrobble-backfill/internal/journal"
	"github.com/wesback/scrobble-backfill/internal/lastfm"
	"github.com/wesback/scrobble-backfill/internal/observability"
)

func TestProfileUsePersistsActiveProfile(t *testing.T) {
	store := config.NewFileStore(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Save(config.Config{
		Profiles: map[string]config.Profile{
			"personal": {Name: "personal"},
			"work":     {Name: "work"},
		},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := RunWithStore([]string{"profile", "use", "work"}, &stdout, &stderr, store)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), `active profile set to "work"`) {
		t.Fatalf("stdout = %q, want active-profile confirmation", stdout.String())
	}

	reloaded, err := store.Load()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.ActiveProfile != "work" {
		t.Fatalf("active profile = %q, want work", reloaded.ActiveProfile)
	}
}

func TestProfileUseRejectsUnknownProfile(t *testing.T) {
	store := config.NewFileStore(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Save(config.Config{
		Profiles:      map[string]config.Profile{"personal": {Name: "personal"}},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := RunWithStore([]string{"profile", "use", "missing"}, &stdout, &stderr, store)
	if exitCode == 0 {
		t.Fatal("expected unknown profile to fail")
	}
	if !strings.Contains(stderr.String(), `profile "missing" is not configured`) {
		t.Fatalf("stderr = %q, want actionable unknown-profile error", stderr.String())
	}
	if !strings.Contains(stderr.String(), "personal") {
		t.Fatalf("stderr = %q, want configured profile guidance", stderr.String())
	}
}

func TestGlobalProfileOptionTakesPrecedenceDuringCommandExecution(t *testing.T) {
	store := config.NewFileStore(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Save(config.Config{
		Profiles: map[string]config.Profile{
			"personal": {Name: "personal"},
			"work":     {Name: "work"},
		},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := RunWithStore([]string{"--profile", "work", "profile", "use", "personal"}, &stdout, &stderr, store)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), `active profile set to "work"`) {
		t.Fatalf("stdout = %q, want explicit profile confirmation", stdout.String())
	}
	reloaded, err := store.Load()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.ActiveProfile != "work" {
		t.Fatalf("active profile = %q, want explicit profile work", reloaded.ActiveProfile)
	}
}

func TestProfileOptionTakesPrecedenceOverPersistedActiveProfile(t *testing.T) {
	cfg := config.Config{
		Profiles: map[string]config.Profile{
			"personal": {Name: "personal"},
			"work":     {Name: "work"},
		},
		ActiveProfile: "personal",
	}

	options, _, err := parseArgs([]string{"--profile", "work", "profile", "use", "personal"})
	if err != nil {
		t.Fatalf("parse command: %v", err)
	}
	name, err := cfg.ResolveProfileName(options.profile)
	if err != nil {
		t.Fatalf("resolve profile: %v", err)
	}
	if name != "work" {
		t.Fatalf("resolved profile = %q, want explicit profile work", name)
	}
}

func TestLogLevelOptionsExposeNormalVerboseAndDebug(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want observability.Level
	}{
		{name: "normal", args: []string{"--log-level", "normal", "profile", "use", "work"}, want: observability.LevelNormal},
		{name: "verbose flag", args: []string{"--verbose", "profile", "use", "work"}, want: observability.LevelVerbose},
		{name: "debug flag", args: []string{"--debug", "profile", "use", "work"}, want: observability.LevelDebug},
		{name: "verbose value", args: []string{"--log-level=verbose", "profile", "use", "work"}, want: observability.LevelVerbose},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			options, _, err := parseArgs(test.args)
			if err != nil {
				t.Fatalf("parse args: %v", err)
			}
			if !options.logLevelExplicit {
				t.Fatal("log level was not marked explicit")
			}
			if options.logLevel != test.want {
				t.Fatalf("log level = %v, want %v", options.logLevel, test.want)
			}
		})
	}
}

func TestLogLevelOptionRejectsUnknownLevel(t *testing.T) {
	if _, _, err := parseArgs([]string{"--log-level", "trace", "profile", "use", "work"}); err == nil {
		t.Fatal("expected unknown log level to fail")
	}
}

func TestAnalyseReportsIngestionProgressBeforeSummary(t *testing.T) {
	root := t.TempDir()
	exportPath := filepath.Join(root, "history.json")
	writeGeneratedSpotifyAnalysisExport(t, exportPath, 2500)

	var stdout, stderr bytes.Buffer
	exitCode := RunWithDependencies(
		[]string{"analyse", "--from", "2024-01-02", "--to", "2024-01-02", exportPath},
		&stdout,
		&stderr,
		newAnalyseTestDependencies(t, root),
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}

	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	summaryIndex := indexOfLinePrefix(lines, "Analysis summary for profile")
	if summaryIndex < 0 {
		t.Fatalf("stdout = %q, want analysis summary", stdout.String())
	}

	var updateCounts []int
	lastUpdateIndex := -1
	for index, line := range lines[:summaryIndex] {
		var count, total int
		if _, err := fmt.Sscanf(line, "progress: %d/%d", &count, &total); err == nil {
			updateCounts = append(updateCounts, count)
			lastUpdateIndex = index
		}
	}
	if !reflect.DeepEqual(updateCounts, []int{1000, 2000}) {
		t.Fatalf("progress update counts = %v, want [1000 2000] before summary; stdout = %q",
			updateCounts, stdout.String())
	}
	completionIndex := indexOfLinePrefix(lines, "progress complete:")
	if completionIndex <= lastUpdateIndex || completionIndex >= summaryIndex {
		t.Fatalf("completion line index = %d, last update index = %d, summary index = %d; stdout = %q",
			completionIndex, lastUpdateIndex, summaryIndex, stdout.String())
	}
	if !strings.Contains(lines[completionIndex], "2500 records ingested") {
		t.Fatalf("completion line = %q, want final count of 2500 records", lines[completionIndex])
	}
}

func TestAnalyseReportsCompletionWithoutPeriodicProgressForSmallExport(t *testing.T) {
	root := t.TempDir()
	exportPath := filepath.Join(root, "history.json")
	writeGeneratedSpotifyAnalysisExport(t, exportPath, 3)

	var stdout, stderr bytes.Buffer
	exitCode := RunWithDependencies(
		[]string{"analyse", "--from", "2024-01-02", "--to", "2024-01-02", exportPath},
		&stdout,
		&stderr,
		newAnalyseTestDependencies(t, root),
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}

	lines := strings.Split(strings.TrimSuffix(stdout.String(), "\n"), "\n")
	summaryIndex := indexOfLinePrefix(lines, "Analysis summary for profile")
	if summaryIndex < 0 {
		t.Fatalf("stdout = %q, want analysis summary", stdout.String())
	}
	progressLines := make([]string, 0, 1)
	for _, line := range lines[:summaryIndex] {
		if strings.Contains(line, "progress") {
			progressLines = append(progressLines, line)
		}
	}
	if len(progressLines) != 1 || !strings.HasPrefix(progressLines[0], "progress complete:") {
		t.Fatalf("progress lines before summary = %q, want only the final completion message; stdout = %q",
			progressLines, stdout.String())
	}
	if !strings.Contains(progressLines[0], "3 records ingested") {
		t.Fatalf("completion line = %q, want final count of 3 records", progressLines[0])
	}
}

func TestImportDryRunReportsIngestionProgressBeforeSummary(t *testing.T) {
	stdout := runImportWithGeneratedSpotifyExport(t, 2500)
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	summaryIndex := indexOfLinePrefix(lines, "Import summary for profile")
	if summaryIndex < 0 {
		t.Fatalf("stdout = %q, want import summary", stdout)
	}

	var updateCounts []int
	lastUpdateIndex := -1
	for index, line := range lines[:summaryIndex] {
		var count, total int
		if _, err := fmt.Sscanf(line, "progress: %d/%d", &count, &total); err == nil {
			updateCounts = append(updateCounts, count)
			lastUpdateIndex = index
		}
	}
	if !reflect.DeepEqual(updateCounts, []int{1000, 2000}) {
		t.Fatalf("progress update counts = %v, want [1000 2000] before summary; stdout = %q",
			updateCounts, stdout)
	}
	completionIndex := indexOfLinePrefix(lines, "progress complete:")
	if completionIndex <= lastUpdateIndex || completionIndex >= summaryIndex {
		t.Fatalf("completion line index = %d, last update index = %d, summary index = %d; stdout = %q",
			completionIndex, lastUpdateIndex, summaryIndex, stdout)
	}
	if !strings.Contains(lines[completionIndex], "2500 records ingested") {
		t.Fatalf("completion line = %q, want final count of 2500 records", lines[completionIndex])
	}
}

func TestImportDryRunReportsOnlyCompletionForSmallExport(t *testing.T) {
	stdout := runImportWithGeneratedSpotifyExport(t, 3)
	lines := strings.Split(strings.TrimSuffix(stdout, "\n"), "\n")
	summaryIndex := indexOfLinePrefix(lines, "Import summary for profile")
	if summaryIndex < 0 {
		t.Fatalf("stdout = %q, want import summary", stdout)
	}
	progressLines := make([]string, 0, 1)
	for _, line := range lines[:summaryIndex] {
		if strings.Contains(line, "progress") {
			progressLines = append(progressLines, line)
		}
	}
	if len(progressLines) != 1 || !strings.HasPrefix(progressLines[0], "progress complete:") {
		t.Fatalf("progress lines before summary = %q, want only the final completion message; stdout = %q",
			progressLines, stdout)
	}
	if !strings.Contains(progressLines[0], "3 records ingested") {
		t.Fatalf("completion line = %q, want final count of 3 records", progressLines[0])
	}
}

func runImportWithGeneratedSpotifyExport(t *testing.T, count int) string {
	t.Helper()
	root := t.TempDir()
	exportPath := filepath.Join(root, "history.json")
	writeGeneratedSpotifyAnalysisExport(t, exportPath, count)

	var stdout, stderr bytes.Buffer
	dependencies := newAnalyseTestDependencies(t, root)
	dependencies.JournalStore = journal.NewFileStore(filepath.Join(root, "journal"))
	exitCode := RunWithDependencies(
		[]string{"import", "--from", "2024-01-02", "--to", "2024-01-02", "--dry-run", exportPath},
		&stdout,
		&stderr,
		dependencies,
	)
	if exitCode != 0 {
		t.Fatalf("dry-run exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	return stdout.String()
}

func TestAnalysePreservesIngestionErrorHandling(t *testing.T) {
	root := t.TempDir()
	exportPath := filepath.Join(root, "corrupt.zip")
	if err := os.WriteFile(exportPath, []byte("not a zip archive"), 0o600); err != nil {
		t.Fatalf("write corrupt archive: %v", err)
	}

	var stdout, stderr bytes.Buffer
	exitCode := RunWithDependencies(
		[]string{"analyse", "--from", "2024-01-02", "--to", "2024-01-02", exportPath},
		&stdout,
		&stderr,
		newAnalyseTestDependencies(t, root),
	)
	if exitCode != 1 {
		t.Fatalf("exit code = %d, want 1; stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stderr.String(), "error: analyse: ") {
		t.Fatalf("stderr = %q, want existing analyse error prefix", stderr.String())
	}
	if strings.Contains(stdout.String(), "Analysis summary for profile") {
		t.Fatalf("stdout = %q, unexpected analysis summary after ingestion error", stdout.String())
	}
}

func newAnalyseTestDependencies(t *testing.T, root string) Dependencies {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fmt.Fprint(w, `{"recenttracks":{"track":[],"@attr":{"totalPages":"1"}}}`)
	}))
	t.Cleanup(server.Close)

	store := config.NewFileStore(filepath.Join(root, "config.json"))
	if err := store.Save(config.Config{
		Profiles:      map[string]config.Profile{"personal": {Name: "personal", LastFMUsername: "alice"}},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	client := lastfm.NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	return Dependencies{
		ConfigStore:     store,
		CredentialStore: &commandCredentialStore{sessions: map[string]string{"personal": "session"}},
		LastFMClient:    client,
	}
}

func writeGeneratedSpotifyAnalysisExport(t *testing.T, path string, count int) {
	t.Helper()
	records := make([]string, count)
	for index := range records {
		records[index] = fmt.Sprintf(
			`{"ts":"2024-01-02T12:00:00Z","platform":"web","ms_played":240000,"master_metadata_track_name":"Track %04d","master_metadata_album_artist_name":"Artist","master_metadata_album_album_name":"Album","spotify_track_uri":"spotify:track:track-%04d"}`,
			index, index,
		)
	}
	writeSpotifyAnalysisExport(t, path, records...)
}

func indexOfLinePrefix(lines []string, prefix string) int {
	for index, line := range lines {
		if strings.HasPrefix(line, prefix) {
			return index
		}
	}
	return -1
}

func TestAnalyseUsesPersistedTimestampToleranceAndPreservesItWhenOverridden(t *testing.T) {
	root := t.TempDir()
	exportPath := filepath.Join(root, "history.json")
	writeSpotifyAnalysisExport(t, exportPath, `{"ts":"2024-01-02T12:00:00Z","platform":"web","ms_played":240000,"master_metadata_track_name":"Track","master_metadata_album_artist_name":"Artist","master_metadata_album_album_name":"Album","spotify_track_uri":"spotify:track:track"}`)

	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		methods = append(methods, r.PostForm.Get("method"))
		fmt.Fprint(w, `{"recenttracks":{"track":[],"@attr":{"totalPages":"1"}}}`)
	}))
	defer server.Close()

	store := config.NewFileStore(filepath.Join(root, "config.json"))
	if err := store.Save(config.Config{
		Profiles:           map[string]config.Profile{"personal": {Name: "personal", LastFMUsername: "alice"}},
		ActiveProfile:      "personal",
		TimestampTolerance: 25 * time.Second,
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	var stdout, stderr bytes.Buffer
	client := lastfm.NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	exitCode := RunWithDependencies(
		[]string{"analyse", "--from", "2024-01-02", "--to", "2024-01-02", exportPath},
		&stdout,
		&stderr,
		Dependencies{
			ConfigStore:     store,
			CredentialStore: &commandCredentialStore{sessions: map[string]string{"personal": "session"}},
			LastFMClient:    client,
		},
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Timestamp tolerance: 25s") {
		t.Fatalf("stdout = %q, want persisted tolerance", stdout.String())
	}
	if len(methods) != 1 || methods[0] != "user.getRecentTracks" {
		t.Fatalf("Last.fm methods = %#v, want history only", methods)
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = RunWithDependencies(
		[]string{"analyse", "--from", "2024-01-02", "--to", "2024-01-02", "--timestamp-tolerance", "5s", exportPath},
		&stdout,
		&stderr,
		Dependencies{
			ConfigStore:     store,
			CredentialStore: &commandCredentialStore{sessions: map[string]string{"personal": "session"}},
			LastFMClient:    client,
		},
	)
	if exitCode != 0 {
		t.Fatalf("overridden analyse exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Timestamp tolerance: 5s") {
		t.Fatalf("stdout = %q, want invocation override", stdout.String())
	}
	reloaded, err := store.Load()
	if err != nil {
		t.Fatalf("reload config after override: %v", err)
	}
	if reloaded.TimestampTolerance != 25*time.Second {
		t.Fatalf("stored timestamp tolerance = %s, want 25s", reloaded.TimestampTolerance)
	}
}

func TestImportUsesPersistedDefaultsAndCommandLineOverridesWithoutSavingThem(t *testing.T) {
	root := t.TempDir()
	exportPath := filepath.Join(root, "history.json")
	records := make([]string, 51)
	for index := range records {
		records[index] = fmt.Sprintf(`{"ts":"2024-01-02T12:00:00Z","platform":"web","ms_played":240000,"master_metadata_track_name":"Track %02d","master_metadata_album_artist_name":"Artist","master_metadata_album_album_name":"Album","spotify_track_uri":"spotify:track:track-%02d"}`, index, index)
	}
	writeSpotifyAnalysisExport(t, exportPath, records...)

	var submissionCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		switch r.PostForm.Get("method") {
		case "user.getRecentTracks":
			fmt.Fprint(w, `{"recenttracks":{"track":[],"@attr":{"totalPages":"1"}}}`)
		case "track.scrobble":
			submissionCalls++
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("unexpected Last.fm method %q", r.PostForm.Get("method"))
		}
	}))
	defer server.Close()

	configStore := config.NewFileStore(filepath.Join(root, "config.json"))
	if err := configStore.Save(config.Config{
		Profiles:           map[string]config.Profile{"personal": {Name: "personal", LastFMUsername: "alice"}},
		ActiveProfile:      "personal",
		TimestampTolerance: 15 * time.Second,
		BatchDelay:         250 * time.Millisecond,
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	journalStore := journal.NewFileStore(filepath.Join(root, "journal"))
	client := lastfm.NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var delays []time.Duration
	dependencies := Dependencies{
		ConfigStore:     configStore,
		CredentialStore: &commandCredentialStore{sessions: map[string]string{"personal": "session"}},
		LastFMClient:    client,
		JournalStore:    journalStore,
		Submission: lastfm.SubmissionOptions{
			Sleep: func(_ context.Context, delay time.Duration) error {
				delays = append(delays, delay)
				return nil
			},
		},
	}

	var stdout, stderr bytes.Buffer
	args := []string{"import", "--from", "2024-01-02", "--to", "2024-01-02", exportPath}
	if exitCode := RunWithDependencies(args, &stdout, &stderr, dependencies); exitCode != 0 {
		t.Fatalf("persisted-default import exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Timestamp tolerance: 15s") {
		t.Fatalf("stdout = %q, want persisted timestamp tolerance", stdout.String())
	}
	if !reflect.DeepEqual(delays, []time.Duration{250 * time.Millisecond}) {
		t.Fatalf("submission delays = %v, want persisted batch delay", delays)
	}
	if submissionCalls != 2 {
		t.Fatalf("submission calls = %d, want two batches", submissionCalls)
	}

	stdout.Reset()
	stderr.Reset()
	delays = nil
	overrideArgs := []string{"import", "--from", "2024-01-02", "--to", "2024-01-02", "--timestamp-tolerance", "5s", "--batch-delay", "0s", exportPath}
	if exitCode := RunWithDependencies(overrideArgs, &stdout, &stderr, dependencies); exitCode != 0 {
		t.Fatalf("overridden import exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Timestamp tolerance: 5s") {
		t.Fatalf("stdout = %q, want timestamp override", stdout.String())
	}
	if len(delays) != 0 {
		t.Fatalf("submission delays after zero override = %v, want none", delays)
	}
	if submissionCalls != 4 {
		t.Fatalf("submission calls after override = %d, want four total batches", submissionCalls)
	}
	reloaded, err := configStore.Load()
	if err != nil {
		t.Fatalf("reload config after import overrides: %v", err)
	}
	if reloaded.TimestampTolerance != 15*time.Second || reloaded.BatchDelay != 250*time.Millisecond {
		t.Fatalf("stored defaults after overrides = tolerance %s, delay %s; want 15s and 250ms", reloaded.TimestampTolerance, reloaded.BatchDelay)
	}
}

func TestAnalyseReportsProfileBoundsConfidenceAndExclusionsWithoutMutatingState(t *testing.T) {
	root := t.TempDir()
	exportPath := filepath.Join(root, "history.json")
	writeSpotifyAnalysisExport(t, exportPath,
		`{"ts":"2024-01-02T12:00:00Z","platform":"web","ms_played":240000,"master_metadata_track_name":"Exact","master_metadata_album_artist_name":"Artist","master_metadata_album_album_name":"Album","spotify_track_uri":"spotify:track:exact"}`,
		`{"ts":"2024-01-02T12:00:00Z","platform":"web","ms_played":240000,"master_metadata_track_name":"Medium","master_metadata_album_artist_name":"Artist","master_metadata_album_album_name":"Album","spotify_track_uri":"spotify:track:medium"}`,
		`{"ts":"2024-01-02T12:00:00Z","platform":"web","ms_played":240000,"master_metadata_track_name":"Missing","master_metadata_album_artist_name":"Artist","master_metadata_album_album_name":"Album","spotify_track_uri":"spotify:track:missing"}`,
		`{"ts":"2024-01-02T12:00:00Z","platform":"web","ms_played":240000,"episode_name":"Episode","episode_show_name":"Show","spotify_episode_uri":"spotify:episode:episode"}`,
		`{"ts":"2024-01-02T12:00:00Z","platform":"web","ms_played":240000,"master_metadata_track_name":"Offline","master_metadata_album_artist_name":"Artist","master_metadata_album_album_name":"Album","spotify_track_uri":"spotify:track:offline","offline":true}`,
	)

	var methods []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		method := r.PostForm.Get("method")
		methods = append(methods, method)
		if method == "track.scrobble" || method == "track.updateNowPlaying" {
			t.Errorf("analyse made a submission request: %q", method)
		}
		if method != "user.getRecentTracks" {
			fmt.Fprint(w, `{"recenttracks":{"track":[],"@attr":{"totalPages":"1"}}}`)
			return
		}
		if got := r.PostForm.Get("user"); got != "work-user" {
			t.Errorf("history user = %q, want work-user", got)
		}
		if got := r.PostForm.Get("sk"); got != "work-session" {
			t.Errorf("history session = %q, want work-session", got)
		}
		localStart := time.Date(2024, 1, 2, 0, 0, 0, 0, time.Local).UTC()
		localEnd := time.Date(2024, 1, 3, 0, 0, 0, 0, time.Local).UTC().Add(-time.Nanosecond)
		if got := r.PostForm.Get("from"); got != fmt.Sprint(localStart.Unix()) {
			t.Errorf("history from = %q, want %d", got, localStart.Unix())
		}
		if got := r.PostForm.Get("to"); got != fmt.Sprint(localEnd.Unix()) {
			t.Errorf("history to = %q, want %d", got, localEnd.Unix())
		}
		base := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC)
		fmt.Fprintf(w, `{"recenttracks":{"track":[{"artist":{"#text":"Artist"},"name":"Exact","date":{"uts":"%d"}},{"artist":{"#text":"Artist"},"name":"Medium","date":{"uts":"%d"}}],"@attr":{"totalPages":"1"}}}`, base.Unix(), base.Add(-10*time.Second).Unix())
	}))
	defer server.Close()

	store := config.NewFileStore(filepath.Join(root, "config.json"))
	if err := store.Save(config.Config{
		Profiles: map[string]config.Profile{
			"personal": {Name: "personal", LastFMUsername: "personal-user"},
			"work":     {Name: "work", LastFMUsername: "work-user"},
		},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	before, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("read initial config: %v", err)
	}

	var stdout, stderr bytes.Buffer
	client := lastfm.NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	exitCode := RunWithDependencies(
		[]string{"--profile", "work", "analyse", "--from=2024-01-02", "--to=2024-01-02", "--timestamp-tolerance", "20s", exportPath},
		&stdout,
		&stderr,
		Dependencies{
			ConfigStore: store,
			CredentialStore: &commandCredentialStore{sessions: map[string]string{
				"personal": "personal-session",
				"work":     "work-session",
			}},
			LastFMClient: client,
		},
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	output := stdout.String()
	for _, want := range []string{
		`Analysis summary for profile "work"`,
		"Total Spotify plays: 5",
		"In-scope Last.fm scrobbles: 2",
		"Estimated missing plays: 1",
		"Covered date range: 2024-01-02 to 2024-01-02",
		"Timestamp tolerance: 20s",
		"Confidence: high=1 medium=1 low=1",
		"excluded_podcast: 1",
		"excluded_local_or_offline: 1",
	} {
		if !strings.Contains(output, want) {
			t.Fatalf("stdout = %q, want %q", output, want)
		}
	}
	after, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("read final config: %v", err)
	}
	if string(after) != string(before) {
		t.Fatalf("analyse mutated config: before %q, after %q", before, after)
	}
	if _, err := os.Stat(filepath.Join(root, "journal")); !os.IsNotExist(err) {
		t.Fatalf("analyse created journal state: stat error = %v", err)
	}
	if len(methods) == 0 {
		t.Fatal("analyse did not request Last.fm history")
	}
}

func TestImportRunsLiveComparisonSkipsDuplicatesAndResolvesProfileAndBounds(t *testing.T) {
	root := t.TempDir()
	exportPath := filepath.Join(root, "history.json")
	base := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC)
	writeSpotifyAnalysisExport(t, exportPath,
		fmt.Sprintf(`{"ts":%q,"platform":"web","ms_played":240000,"master_metadata_track_name":"Duplicate","master_metadata_album_artist_name":"Artist","master_metadata_album_album_name":"Album","spotify_track_uri":"spotify:track:duplicate"}`, base.Format(time.RFC3339)),
		fmt.Sprintf(`{"ts":%q,"platform":"web","ms_played":240000,"master_metadata_track_name":"Missing","master_metadata_album_artist_name":"Artist","master_metadata_album_album_name":"Album","spotify_track_uri":"spotify:track:missing"}`, base.Format(time.RFC3339)),
	)

	var historyCalls, submissionCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		switch r.PostForm.Get("method") {
		case "user.getRecentTracks":
			historyCalls++
			if got := r.PostForm.Get("user"); got != "work-user" {
				t.Errorf("history user = %q, want work-user", got)
			}
			if got := r.PostForm.Get("from"); got != fmt.Sprint(time.Date(2024, 1, 2, 0, 0, 0, 0, time.Local).UTC().Unix()) {
				t.Errorf("history from = %q, want local start", got)
			}
			if got := r.PostForm.Get("to"); got != fmt.Sprint(time.Date(2024, 1, 3, 0, 0, 0, 0, time.Local).UTC().Add(-time.Nanosecond).Unix()) {
				t.Errorf("history to = %q, want inclusive local end", got)
			}
			if historyCalls <= 2 {
				fmt.Fprintf(w, `{"recenttracks":{"track":[{"artist":{"#text":"Artist"},"name":"Duplicate","date":{"uts":"%d"}}],"@attr":{"totalPages":"1"}}}`, base.Unix())
				return
			}
			fmt.Fprintf(w, `{"recenttracks":{"track":[{"artist":{"#text":"Artist"},"name":"Duplicate","date":{"uts":"%d"}},{"artist":{"#text":"Artist"},"name":"Missing","date":{"uts":"%d"}}],"@attr":{"totalPages":"1"}}}`, base.Unix(), base.Unix())
		case "track.scrobble":
			submissionCalls++
			if got := r.PostForm.Get("track[0]"); got != "Missing" {
				t.Errorf("submitted track = %q, want Missing", got)
			}
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("unexpected Last.fm method %q", r.PostForm.Get("method"))
			fmt.Fprint(w, `{}`)
		}
	}))
	defer server.Close()

	store := config.NewFileStore(filepath.Join(root, "config.json"))
	if err := store.Save(config.Config{
		Profiles: map[string]config.Profile{
			"personal": {Name: "personal", LastFMUsername: "personal-user"},
			"work":     {Name: "work", LastFMUsername: "work-user"},
		},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	client := lastfm.NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	args := []string{"--profile", "work", "import", "--from", "2024-01-02", "--to", "2024-01-02", exportPath}
	dependencies := Dependencies{
		ConfigStore:     store,
		CredentialStore: &commandCredentialStore{sessions: map[string]string{"work": "work-session"}},
		LastFMClient:    client,
		JournalStore:    journal.NewFileStore(filepath.Join(root, "journal")),
		Submission:      lastfm.SubmissionOptions{BaselineDelay: 0},
	}

	var stdout, stderr bytes.Buffer
	if exitCode := RunWithDependencies(args, &stdout, &stderr, dependencies); exitCode != 0 {
		t.Fatalf("first import exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Skipped duplicates: 1") || !strings.Contains(stdout.String(), "Missing plays: 1") {
		t.Fatalf("first import output = %q, want duplicate and missing counts", stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	if exitCode := RunWithDependencies(args, &stdout, &stderr, dependencies); exitCode != 0 {
		t.Fatalf("second import exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), "Skipped duplicates: 2") || !strings.Contains(stdout.String(), "Nothing to submit.") {
		t.Fatalf("second import output = %q, want fresh live comparison counts", stdout.String())
	}
	if historyCalls != 4 {
		t.Fatalf("history calls = %d, want history retrieval during both live comparisons", historyCalls)
	}
	if submissionCalls != 1 {
		t.Fatalf("submission calls = %d, want only the first missing play submitted", submissionCalls)
	}
}

func TestImportDryRunPlansBatchesWithoutSubmissionOrMarkingSubmitted(t *testing.T) {
	root := t.TempDir()
	exportPath := filepath.Join(root, "history.json")
	writeSpotifyAnalysisExport(t, exportPath, `{"ts":"2024-01-02T12:00:00Z","platform":"web","ms_played":240000,"master_metadata_track_name":"Missing","master_metadata_album_artist_name":"Artist","master_metadata_album_album_name":"Album","spotify_track_uri":"spotify:track:missing"}`)
	var submissionCalls int
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		switch r.PostForm.Get("method") {
		case "user.getRecentTracks":
			fmt.Fprint(w, `{"recenttracks":{"track":[],"@attr":{"totalPages":"1"}}}`)
		case "track.scrobble":
			submissionCalls++
			fmt.Fprint(w, `{}`)
		default:
			t.Errorf("unexpected Last.fm method %q", r.PostForm.Get("method"))
		}
	}))
	defer server.Close()
	configStore := config.NewFileStore(filepath.Join(root, "config.json"))
	if err := configStore.Save(config.Config{
		Profiles:      map[string]config.Profile{"personal": {Name: "personal", LastFMUsername: "alice"}},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	journalStore := journal.NewFileStore(filepath.Join(root, "journal"))
	client := lastfm.NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var stdout, stderr bytes.Buffer
	exitCode := RunWithDependencies(
		[]string{"import", "--from", "2024-01-02", "--to", "2024-01-02", "--dry-run", exportPath},
		&stdout,
		&stderr,
		Dependencies{
			ConfigStore:     configStore,
			CredentialStore: &commandCredentialStore{sessions: map[string]string{"personal": "session"}},
			LastFMClient:    client,
			JournalStore:    journalStore,
		},
	)
	if exitCode != 0 {
		t.Fatalf("dry-run exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if submissionCalls != 0 {
		t.Fatalf("submission calls = %d, want zero for dry-run", submissionCalls)
	}
	runs, err := journalStore.ListRuns("personal")
	if err != nil {
		t.Fatalf("list dry-run journal: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("dry-run runs = %d, want one planned run", len(runs))
	}
	resume, err := journalStore.Resume("personal", runs[0].InvocationID)
	if err != nil {
		t.Fatalf("resume dry-run journal: %v", err)
	}
	if len(resume.Pending) != 1 || len(resume.Submitted) != 0 {
		t.Fatalf("dry-run journal = submitted %#v pending %#v, want one planned batch and none submitted", resume.Submitted, resume.Pending)
	}
}

func TestReportSelectsExactlyOneFormatFromJournalRun(t *testing.T) {
	root := t.TempDir()
	configStore := config.NewFileStore(filepath.Join(root, "config.json"))
	if err := configStore.Save(config.Config{
		Profiles:      map[string]config.Profile{"personal": {Name: "personal"}},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	journalStore := journal.NewFileStore(filepath.Join(root, "journal"))
	run, err := journalStore.CreateRun("personal", "import-report")
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := journalStore.SetRunSettings("personal", run.InvocationID, journal.RunSettings{
		From:                  time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
		To:                    time.Date(2024, 1, 2, 0, 0, 0, 0, time.UTC),
		TimestampTolerance:    20 * time.Second,
		TimestampToleranceSet: true,
		EligibilityRule:       "test eligibility rule",
		BatchDelay:            150 * time.Millisecond,
		BatchDelaySet:         true,
	}); err != nil {
		t.Fatalf("set run settings: %v", err)
	}
	if err := journalStore.RecordEvents("personal", run.InvocationID, []journal.Event{
		{Type: "ingestion.warning", Data: map[string]string{
			"input": "bad.json", "code": "malformed_record", "reason": "malformed Spotify input",
		}},
		{Type: "comparison.matched", Data: map[string]string{"count": "1"}},
	}); err != nil {
		t.Fatalf("record events: %v", err)
	}
	if _, err := journalStore.PlanBatch("personal", run.InvocationID, []journal.Submission{{
		Artist: "Artist", Track: "Track", Timestamp: time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC),
	}}); err != nil {
		t.Fatalf("plan batch: %v", err)
	}
	if err := journalStore.MarkSubmitted("personal", run.InvocationID, 1); err != nil {
		t.Fatalf("submit batch: %v", err)
	}

	for _, test := range []struct {
		name string
		flag string
		want string
	}{
		{name: "json", flag: "--json", want: `"imported_scrobbles": 1`},
		{name: "csv", flag: "--csv", want: "imported_scrobbles,1"},
		{name: "html", flag: "--html", want: `data-field="imported_scrobbles"`},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exitCode := RunWithDependencies(
				[]string{"report", test.flag, run.InvocationID},
				&stdout, &stderr,
				Dependencies{ConfigStore: configStore, JournalStore: journalStore},
			)
			if exitCode != 0 {
				t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), test.want) {
				t.Fatalf("report = %q, want %q", stdout.String(), test.want)
			}
			for _, value := range []string{"personal", "2024-01-02", "20s", "150ms", "malformed Spotify input"} {
				if !strings.Contains(stdout.String(), value) {
					t.Fatalf("report = %q, want %q", stdout.String(), value)
				}
			}
		})
	}
	var currentStdout, currentStderr bytes.Buffer
	if exitCode := RunWithDependencies(
		[]string{"report", "--json"},
		&currentStdout, &currentStderr,
		Dependencies{ConfigStore: configStore, JournalStore: journalStore},
	); exitCode != 0 || !strings.Contains(currentStdout.String(), `"invocation_id": "import-report"`) {
		t.Fatalf("current journal report exit code = %d, stdout=%q stderr=%q", exitCode, currentStdout.String(), currentStderr.String())
	}

	var stdout, stderr bytes.Buffer
	if exitCode := RunWithDependencies(
		[]string{"report", "--json", "--csv", run.InvocationID},
		&stdout, &stderr, Dependencies{ConfigStore: configStore, JournalStore: journalStore},
	); exitCode != 2 || !strings.Contains(stderr.String(), "exactly one") {
		t.Fatalf("multiple formats exit code = %d, stderr = %q; want usage error", exitCode, stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if exitCode := RunWithDependencies(
		[]string{"report", run.InvocationID},
		&stdout, &stderr, Dependencies{ConfigStore: configStore, JournalStore: journalStore},
	); exitCode != 2 || !strings.Contains(stderr.String(), "exactly one") {
		t.Fatalf("missing format exit code = %d, stderr = %q; want usage error", exitCode, stderr.String())
	}
}

func TestImportSurfacesJournalEventRecordingFailure(t *testing.T) {
	root := t.TempDir()
	exportPath := filepath.Join(root, "history.json")
	writeSpotifyAnalysisExport(t, exportPath, `{"ts":"2024-01-02T12:00:00Z","platform":"web","ms_played":240000,"master_metadata_track_name":"Missing","master_metadata_album_artist_name":"Artist","master_metadata_album_album_name":"Album","spotify_track_uri":"spotify:track:missing"}`)
	configStore := config.NewFileStore(filepath.Join(root, "config.json"))
	if err := configStore.Save(config.Config{
		Profiles:      map[string]config.Profile{"personal": {Name: "personal", LastFMUsername: "alice"}},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		fmt.Fprint(w, `{"recenttracks":{"track":[],"@attr":{"totalPages":"1"}}}`)
	}))
	defer server.Close()
	client := lastfm.NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	store := &failingEventJournalStore{FileStore: journal.NewFileStore(filepath.Join(root, "journal"))}
	var stdout, stderr bytes.Buffer
	exitCode := RunWithDependencies(
		[]string{"import", "--from", "2024-01-02", "--to", "2024-01-02", "--dry-run", exportPath},
		&stdout, &stderr,
		Dependencies{
			ConfigStore:     configStore,
			CredentialStore: &commandCredentialStore{sessions: map[string]string{"personal": "session-secret"}},
			LastFMClient:    client,
			JournalStore:    store,
		},
	)
	if exitCode != 1 {
		t.Fatalf("exit code = %d, stdout=%q stderr=%q; want recording failure", exitCode, stdout.String(), stderr.String())
	}
	if !strings.Contains(stderr.String(), "record import outcome") ||
		!strings.Contains(stderr.String(), "event write failed") {
		t.Fatalf("stderr = %q, want surfaced event recording failure", stderr.String())
	}
}

func TestVerifyReportsConfirmedAndMissingJournalEntriesReadOnly(t *testing.T) {
	base := time.Date(2024, 1, 2, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name             string
		history          string
		wantConfirmed    string
		wantAbsent       string
		wantAbsentDetail string
	}{
		{
			name:          "fully confirmed",
			history:       fmt.Sprintf(`{"recenttracks":{"track":[{"artist":{"#text":"Artist One"},"name":"Track One","date":{"uts":"%d"}},{"artist":{"#text":"Artist Two"},"name":"Track Two","date":{"uts":"%d"}}],"@attr":{"totalPages":"1"}}}`, base.Unix(), base.Add(time.Minute).Unix()),
			wantConfirmed: "Confirmed journaled scrobbles: 2",
			wantAbsent:    "Absent journaled scrobbles: 0",
		},
		{
			name:             "partially missing",
			history:          fmt.Sprintf(`{"recenttracks":{"track":[{"artist":{"#text":"Artist One"},"name":"Track One","date":{"uts":"%d"}}],"@attr":{"totalPages":"1"}}}`, base.Unix()),
			wantConfirmed:    "Confirmed journaled scrobbles: 1",
			wantAbsent:       "Absent journaled scrobbles: 1",
			wantAbsentDetail: `"Artist Two" - "Track Two"`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := config.NewFileStore(filepath.Join(root, "config.json"))
			if err := store.Save(config.Config{
				Profiles:      map[string]config.Profile{"personal": {Name: "personal", LastFMUsername: "alice"}},
				ActiveProfile: "personal",
			}); err != nil {
				t.Fatalf("seed config: %v", err)
			}
			journalStore := journal.NewFileStore(filepath.Join(root, "journal"))
			run, err := journalStore.CreateRun("personal", "import-verify")
			if err != nil {
				t.Fatalf("create run: %v", err)
			}
			payloads := []journal.Submission{
				{Artist: "Artist One", Track: "Track One", Timestamp: base},
				{Artist: "Artist Two", Track: "Track Two", Timestamp: base.Add(time.Minute)},
			}
			if _, err := journalStore.PlanBatch("personal", run.InvocationID, payloads); err != nil {
				t.Fatalf("plan batch: %v", err)
			}
			if err := journalStore.MarkSubmitted("personal", run.InvocationID, 1); err != nil {
				t.Fatalf("mark submitted: %v", err)
			}
			before, err := journalStore.OpenRun("personal", run.InvocationID)
			if err != nil {
				t.Fatalf("read journal before verify: %v", err)
			}

			var methods []string
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("parse form: %v", err)
					return
				}
				methods = append(methods, r.PostForm.Get("method"))
				if r.PostForm.Get("method") != "user.getRecentTracks" {
					t.Errorf("method = %q, want history only", r.PostForm.Get("method"))
				}
				if got := r.PostForm.Get("from"); got != fmt.Sprint(base.Unix()) {
					t.Errorf("history from = %q, want %d", got, base.Unix())
				}
				if got := r.PostForm.Get("to"); got != fmt.Sprint(base.Add(time.Minute).Unix()) {
					t.Errorf("history to = %q, want %d", got, base.Add(time.Minute).Unix())
				}
				fmt.Fprint(w, test.history)
			}))
			defer server.Close()

			client := lastfm.NewClient("app-key", "app-secret")
			client.BaseURL = server.URL
			var stdout, stderr bytes.Buffer
			exitCode := RunWithDependencies(
				[]string{"verify", run.InvocationID},
				&stdout,
				&stderr,
				Dependencies{
					ConfigStore:     store,
					CredentialStore: &commandCredentialStore{sessions: map[string]string{"personal": "session-secret"}},
					LastFMClient:    client,
					JournalStore:    journalStore,
				},
			)
			if exitCode != 0 {
				t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
			}
			output := stdout.String()
			for _, want := range []string{test.wantConfirmed, test.wantAbsent} {
				if !strings.Contains(output, want) {
					t.Fatalf("stdout = %q, want %q", output, want)
				}
			}
			if test.wantAbsentDetail != "" && !strings.Contains(output, test.wantAbsentDetail) {
				t.Fatalf("stdout = %q, want absent journal detail %q", output, test.wantAbsentDetail)
			}
			if !reflect.DeepEqual(methods, []string{"user.getRecentTracks"}) {
				t.Fatalf("Last.fm methods = %#v, want one history request", methods)
			}
			after, err := journalStore.OpenRun("personal", run.InvocationID)
			if err != nil {
				t.Fatalf("read journal after verify: %v", err)
			}
			if !reflect.DeepEqual(after, before) {
				t.Fatalf("verify changed journal: before %#v, after %#v", before, after)
			}
		})
	}
}

func TestVerifyReportsHistoryAndMalformedResponseFailuresWithoutCredential(t *testing.T) {
	const sessionKey = "verify-session-secret"
	tests := []struct {
		name     string
		handler  func(http.ResponseWriter)
		wantText string
	}{
		{
			name: "history failure",
			handler: func(w http.ResponseWriter) {
				http.Error(w, "history unavailable", http.StatusBadGateway)
			},
			wantText: "Last.fm history request",
		},
		{
			name: "malformed response",
			handler: func(w http.ResponseWriter) {
				fmt.Fprint(w, `{"recenttracks":{"track":[{"artist":{"#text":"Artist"},"name":"Track","date":{"uts":"not-a-timestamp"}}],"@attr":{"totalPages":"1"}}}`)
			},
			wantText: "malformed response",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := config.NewFileStore(filepath.Join(root, "config.json"))
			if err := store.Save(config.Config{
				Profiles:      map[string]config.Profile{"personal": {Name: "personal", LastFMUsername: "alice"}},
				ActiveProfile: "personal",
			}); err != nil {
				t.Fatalf("seed config: %v", err)
			}
			journalStore := journal.NewFileStore(filepath.Join(root, "journal"))
			run, err := journalStore.CreateRun("personal", "import-failure")
			if err != nil {
				t.Fatalf("create run: %v", err)
			}
			if _, err := journalStore.PlanBatch("personal", run.InvocationID, []journal.Submission{{
				Artist: "Artist", Track: "Track", Timestamp: time.Unix(100, 0).UTC(),
			}}); err != nil {
				t.Fatalf("plan batch: %v", err)
			}
			if err := journalStore.MarkSubmitted("personal", run.InvocationID, 1); err != nil {
				t.Fatalf("mark submitted: %v", err)
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("parse form: %v", err)
					return
				}
				if method := r.PostForm.Get("method"); method != "user.getRecentTracks" {
					t.Errorf("method = %q, want history only", method)
				}
				test.handler(w)
			}))
			defer server.Close()
			client := lastfm.NewClient("app-key", "app-secret")
			client.BaseURL = server.URL
			var stdout, stderr bytes.Buffer
			exitCode := RunWithDependencies(
				[]string{"verify", run.InvocationID},
				&stdout,
				&stderr,
				Dependencies{
					ConfigStore:     store,
					CredentialStore: &commandCredentialStore{sessions: map[string]string{"personal": sessionKey}},
					LastFMClient:    client,
					JournalStore:    journalStore,
				},
			)
			if exitCode == 0 {
				t.Fatal("verify unexpectedly succeeded")
			}
			if !strings.Contains(stderr.String(), test.wantText) {
				t.Fatalf("stderr = %q, want %q", stderr.String(), test.wantText)
			}
			if strings.Contains(stderr.String(), sessionKey) {
				t.Fatalf("stderr exposes session credential: %q", stderr.String())
			}
			if strings.Contains(stdout.String(), sessionKey) {
				t.Fatalf("stdout exposes session credential: %q", stdout.String())
			}
		})
	}
}
func TestImportConfirmationThresholdAndYesOverride(t *testing.T) {
	tests := []struct {
		name       string
		count      int
		args       []string
		input      string
		wantExit   int
		wantPrompt bool
	}{
		{name: "rejection above threshold", count: LargeImportConfirmationThreshold + 1, input: "n\n", wantExit: 1, wantPrompt: true},
		{name: "acceptance above threshold", count: LargeImportConfirmationThreshold + 1, input: "y\n", wantExit: 0, wantPrompt: true},
		{name: "threshold boundary does not prompt", count: LargeImportConfirmationThreshold, input: "n\n", wantExit: 0, wantPrompt: false},
		{name: "yes bypasses prompt", count: LargeImportConfirmationThreshold + 1, args: []string{"--yes"}, input: "n\n", wantExit: 0, wantPrompt: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			exportPath := filepath.Join(root, "history.json")
			records := make([]string, test.count)
			for index := range records {
				records[index] = `{"ts":"2024-01-02T12:00:00Z","platform":"web","ms_played":240000,"master_metadata_track_name":"Track","master_metadata_album_artist_name":"Artist","master_metadata_album_album_name":"Album","spotify_track_uri":"spotify:track:track"}`
			}
			writeSpotifyAnalysisExport(t, exportPath, records...)
			var submissionCalls int
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if err := r.ParseForm(); err != nil {
					t.Errorf("parse form: %v", err)
					return
				}
				switch r.PostForm.Get("method") {
				case "user.getRecentTracks":
					fmt.Fprint(w, `{"recenttracks":{"track":[],"@attr":{"totalPages":"1"}}}`)
				case "track.scrobble":
					submissionCalls++
					fmt.Fprint(w, `{}`)
				default:
					t.Errorf("unexpected Last.fm method %q", r.PostForm.Get("method"))
				}
			}))
			defer server.Close()
			configStore := config.NewFileStore(filepath.Join(root, "config.json"))
			if err := configStore.Save(config.Config{
				Profiles:      map[string]config.Profile{"personal": {Name: "personal", LastFMUsername: "alice"}},
				ActiveProfile: "personal",
			}); err != nil {
				t.Fatalf("seed config: %v", err)
			}
			client := lastfm.NewClient("app-key", "app-secret")
			client.BaseURL = server.URL
			args := append([]string{"import", "--from", "2024-01-02", "--to", "2024-01-02"}, test.args...)
			args = append(args, exportPath)
			var stdout, stderr bytes.Buffer
			exitCode := RunWithDependencies(args, &stdout, &stderr, Dependencies{
				ConfigStore:     configStore,
				CredentialStore: &commandCredentialStore{sessions: map[string]string{"personal": "session"}},
				LastFMClient:    client,
				JournalStore:    journal.NewFileStore(filepath.Join(root, "journal")),
				Submission:      lastfm.SubmissionOptions{BaselineDelay: 0},
				Input:           strings.NewReader(test.input),
			})
			if exitCode != test.wantExit {
				t.Fatalf("exit code = %d, want %d; stdout=%q stderr=%q", exitCode, test.wantExit, stdout.String(), stderr.String())
			}
			gotPrompt := strings.Contains(stdout.String(), "requires confirmation")
			if gotPrompt != test.wantPrompt {
				t.Fatalf("prompt shown = %v, want %v; stdout=%q", gotPrompt, test.wantPrompt, stdout.String())
			}
			if test.wantExit == 1 && submissionCalls != 0 {
				t.Fatalf("rejected import made %d submission calls", submissionCalls)
			}
			if test.wantExit == 0 && submissionCalls == 0 {
				t.Fatal("accepted import made no submission calls")
			}
		})
	}
}

func writeSpotifyAnalysisExport(t *testing.T, path string, records ...string) {
	t.Helper()
	data := "[" + strings.Join(records, ",") + "]"
	if err := os.WriteFile(path, []byte(data), 0o600); err != nil {
		t.Fatalf("write Spotify export: %v", err)
	}
}

type commandCredentialStore struct {
	sessions  map[string]string
	saveErr   error
	loadErr   error
	deleteErr error
	hasErr    error
}

type failingEventJournalStore struct {
	*journal.FileStore
}

func (s *failingEventJournalStore) RecordEvent(string, string, journal.Event) error {
	return errors.New("event write failed")
}

func (s *failingEventJournalStore) RecordEvents(string, string, []journal.Event) error {
	return errors.New("event write failed")
}

func (s *commandCredentialStore) Save(profile, session string) error {
	if s.saveErr != nil {
		return s.saveErr
	}
	if s.sessions == nil {
		s.sessions = make(map[string]string)
	}
	s.sessions[profile] = session
	return nil
}

func (s *commandCredentialStore) Load(profile string) (string, error) {
	if s.loadErr != nil {
		return "", s.loadErr
	}
	session, ok := s.sessions[profile]
	if !ok {
		return "", credentials.ErrCredentialNotFound
	}
	return session, nil
}

func (s *commandCredentialStore) Delete(profile string) error {
	if s.deleteErr != nil {
		return s.deleteErr
	}
	delete(s.sessions, profile)
	return nil
}

func (s *commandCredentialStore) Has(profile string) (bool, error) {
	if s.hasErr != nil {
		return false, s.hasErr
	}
	_, ok := s.sessions[profile]
	return ok, nil
}

func TestLoginStoresSessionAndActivatesFirstProfile(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("request method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		switch r.PostForm.Get("method") {
		case "auth.getToken":
			fmt.Fprint(w, `{"token":"login-token"}`)
		case "auth.getSession":
			if got := r.PostForm.Get("token"); got != "login-token" {
				t.Errorf("auth.getSession token = %q, want login-token", got)
			}
			fmt.Fprint(w, `{"session":{"name":"alice","key":"session-secret"}}`)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	store := config.NewFileStore(filepath.Join(t.TempDir(), "config.json"))
	credentialsStore := &commandCredentialStore{}
	client := lastfm.NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	var openedURL string
	client.OpenURL = func(rawURL string) error {
		openedURL = rawURL
		return errors.New("browser unavailable")
	}
	var stdout, stderr bytes.Buffer
	exitCode := RunWithDependencies(
		[]string{"--debug", "--profile", "personal", "login"},
		&stdout,
		&stderr,
		Dependencies{
			ConfigStore:     store,
			CredentialStore: credentialsStore,
			LastFMClient:    client,
			Input:           strings.NewReader("\n"),
		},
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := credentialsStore.sessions["personal"]; got != "session-secret" {
		t.Fatalf("stored session = %q, want session-secret", got)
	}
	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("load config: %v", err)
	}
	if cfg.ActiveProfile != "personal" {
		t.Fatalf("active profile = %q, want personal", cfg.ActiveProfile)
	}
	if cfg.Profiles["personal"].LastFMUsername != "alice" {
		t.Fatalf("Last.fm username = %q, want alice", cfg.Profiles["personal"].LastFMUsername)
	}
	authorizationURL, err := url.Parse(openedURL)
	if err != nil {
		t.Fatalf("parse opened authorization URL: %v", err)
	}
	if authorizationURL.Host != "www.last.fm" || authorizationURL.Path != "/api/auth/" {
		t.Fatalf("opened authorization URL = %q, want official Last.fm auth endpoint", openedURL)
	}
	if got := authorizationURL.Query().Get("token"); got != "login-token" {
		t.Fatalf("opened authorization token = %q, want login-token", got)
	}
	if got := authorizationURL.Query().Get("api_key"); got != "app-key" {
		t.Fatalf("opened authorization API key = %q, want app-key", got)
	}
	if !strings.Contains(stdout.String(), openedURL) {
		t.Fatalf("stdout = %q, want manual authorization URL %q", stdout.String(), openedURL)
	}
	if !strings.Contains(stderr.String(), `"event":"lastfm.authorization.browser_open_failed"`) ||
		!strings.Contains(stderr.String(), `"error":"browser unavailable"`) {
		t.Fatalf("stderr = %q, want debug event recording browser opener error", stderr.String())
	}
	configData, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if strings.Contains(string(configData), "session-secret") || strings.Contains(string(configData), "app-secret") || strings.Contains(string(configData), "app-key") {
		t.Fatalf("persisted config exposes a credential: %q", configData)
	}
	if strings.Contains(stdout.String(), "app-secret") || strings.Contains(stdout.String(), "session-secret") {
		t.Fatalf("stdout exposes a credential: %q", stdout.String())
	}
}

func TestLogoutDeletesOnlyResolvedProfileCredential(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want string
	}{
		{name: "active profile", args: []string{"logout"}, want: "personal"},
		{name: "named profile", args: []string{"--profile", "family", "logout"}, want: "family"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := config.NewFileStore(filepath.Join(t.TempDir(), "config.json"))
			if err := store.Save(config.Config{
				Profiles: map[string]config.Profile{
					"personal": {Name: "personal"},
					"family":   {Name: "family"},
				},
				ActiveProfile: "personal",
			}); err != nil {
				t.Fatalf("seed config: %v", err)
			}
			credentialsStore := &commandCredentialStore{sessions: map[string]string{
				"personal": "personal-secret",
				"family":   "family-secret",
			}}
			var stdout, stderr bytes.Buffer
			if exitCode := RunWithDependencies(
				test.args,
				&stdout,
				&stderr,
				Dependencies{ConfigStore: store, CredentialStore: credentialsStore},
			); exitCode != 0 {
				t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
			}
			if _, err := credentialsStore.Load(test.want); !errors.Is(err, credentials.ErrCredentialNotFound) {
				t.Fatalf("%s credential error = %v, want not found", test.want, err)
			}
			for _, profile := range []string{"personal", "family"} {
				if profile == test.want {
					continue
				}
				if got, err := credentialsStore.Load(profile); err != nil || got == "" {
					t.Fatalf("%s credential = %q, %v; want unchanged", profile, got, err)
				}
			}
		})
	}
}

func TestStatusReportsSessionPresenceWithoutCredential(t *testing.T) {
	store := config.NewFileStore(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Save(config.Config{
		Profiles: map[string]config.Profile{
			"personal": {Name: "personal"},
			"family":   {Name: "family"},
		},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	tests := []struct {
		name       string
		args       []string
		sessions   map[string]string
		wantOutput string
	}{
		{name: "active profile with session", args: []string{"status"}, sessions: map[string]string{"personal": "session-secret"}, wantOutput: `profile "personal": logged in`},
		{name: "named profile with session", args: []string{"--profile", "family", "status"}, sessions: map[string]string{"family": "family-session"}, wantOutput: `profile "family": logged in`},
		{name: "named profile without session", args: []string{"--profile", "family", "status"}, sessions: map[string]string{}, wantOutput: `profile "family": not logged in`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var stdout, stderr bytes.Buffer
			exitCode := RunWithDependencies(
				test.args,
				&stdout,
				&stderr,
				Dependencies{ConfigStore: store, CredentialStore: &commandCredentialStore{sessions: test.sessions}},
			)
			if exitCode != 0 {
				t.Fatalf("exit code = %d, stderr = %q", exitCode, stderr.String())
			}
			if !strings.Contains(stdout.String(), test.wantOutput) {
				t.Fatalf("stdout = %q, want %q", stdout.String(), test.wantOutput)
			}
			for _, session := range test.sessions {
				if strings.Contains(stdout.String(), session) {
					t.Fatalf("stdout exposes session credential: %q", stdout.String())
				}
			}
		})
	}
}

func TestLinuxUnavailableNativeStoreUsesFallbackForLoginStatusAndAuthenticatedRequest(t *testing.T) {
	const (
		apiKey     = "app-key"
		apiSecret  = "app-secret"
		sessionKey = "work-session"
	)
	var receivedRequest url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		switch r.PostForm.Get("method") {
		case "auth.getToken":
			fmt.Fprint(w, `{"token":"login-token"}`)
		case "auth.getSession":
			fmt.Fprint(w, `{"session":{"name":"alice","key":"work-session"}}`)
		case "user.getInfo":
			receivedRequest = r.PostForm
			fmt.Fprint(w, `{}`)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	configStore := config.NewFileStore(filepath.Join(t.TempDir(), "config.json"))
	if err := configStore.Save(config.Config{
		Profiles: map[string]config.Profile{
			"personal": {Name: "personal", LastFMUsername: "personal-user"},
			"work":     {Name: "work"},
		},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	unavailable := &credentials.UnavailableError{Cause: errors.New("Secret Service is unavailable")}
	native := &commandCredentialStore{
		loadErr: unavailable,
		saveErr: unavailable,
		hasErr:  unavailable,
	}
	file := &commandCredentialStore{sessions: map[string]string{"personal": "personal-session"}}
	credentialStore := credentials.NewPlatformStore("linux", native, file)
	client := lastfm.NewClient(apiKey, apiSecret)
	client.BaseURL = server.URL
	client.OpenURL = func(string) error { return nil }

	var stdout, stderr bytes.Buffer
	if exitCode := RunWithDependencies(
		[]string{"--profile", "work", "login"},
		&stdout,
		&stderr,
		Dependencies{
			ConfigStore:     configStore,
			CredentialStore: credentialStore,
			LastFMClient:    client,
			Input:           strings.NewReader("\n"),
		},
	); exitCode != 0 {
		t.Fatalf("login exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := file.sessions["work"]; got != sessionKey {
		t.Fatalf("fallback work credential = %q, want %q", got, sessionKey)
	}
	if got := file.sessions["personal"]; got != "personal-session" {
		t.Fatalf("fallback personal credential = %q, want it unchanged", got)
	}

	stdout.Reset()
	stderr.Reset()
	if exitCode := RunWithDependencies(
		[]string{"--profile", "work", "status"},
		&stdout,
		&stderr,
		Dependencies{ConfigStore: configStore, CredentialStore: credentialStore},
	); exitCode != 0 {
		t.Fatalf("status exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if !strings.Contains(stdout.String(), `profile "work": logged in`) {
		t.Fatalf("status output = %q, want work profile logged in", stdout.String())
	}

	options, _, err := parseArgs([]string{"--profile", "work", "status"})
	if err != nil {
		t.Fatalf("parse selected profile: %v", err)
	}
	if _, err := callAuthenticated(
		context.Background(),
		options,
		"user.getInfo",
		nil,
		Dependencies{ConfigStore: configStore, CredentialStore: credentialStore, LastFMClient: client},
	); err != nil {
		t.Fatalf("authenticated fallback request: %v", err)
	}
	if got := receivedRequest.Get("sk"); got != sessionKey {
		t.Fatalf("authenticated request session key = %q, want %q", got, sessionKey)
	}
	if got := receivedRequest.Get("method"); got != "user.getInfo" {
		t.Fatalf("authenticated request method = %q, want user.getInfo", got)
	}
	cfg, err := configStore.Load()
	if err != nil {
		t.Fatalf("load config after selected-profile login: %v", err)
	}
	if cfg.ActiveProfile != "personal" {
		t.Fatalf("active profile after work login = %q, want personal", cfg.ActiveProfile)
	}
	if cfg.Profiles["work"].LastFMUsername != "alice" {
		t.Fatalf("work Last.fm username = %q, want alice", cfg.Profiles["work"].LastFMUsername)
	}
}

func TestLinuxLoginAfterLogoutStoresInRestoredNativeTier(t *testing.T) {
	const (
		apiKey    = "app-key"
		apiSecret = "app-secret"
	)
	sessionNumber := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		switch r.PostForm.Get("method") {
		case "auth.getToken":
			fmt.Fprint(w, `{"token":"login-token"}`)
		case "auth.getSession":
			sessionNumber++
			fmt.Fprintf(w, `{"session":{"name":"alice","key":"session-%d"}}`, sessionNumber)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	configStore := config.NewFileStore(filepath.Join(t.TempDir(), "config.json"))
	if err := configStore.Save(config.Config{
		Profiles:      map[string]config.Profile{"personal": {Name: "personal"}},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	unavailable := &credentials.UnavailableError{Cause: errors.New("Secret Service is unavailable")}
	native := &commandCredentialStore{loadErr: unavailable, saveErr: unavailable, deleteErr: unavailable}
	fallback := &commandCredentialStore{}
	store := credentials.NewPlatformStore("linux", native, fallback)
	client := lastfm.NewClient(apiKey, apiSecret)
	client.BaseURL = server.URL
	client.OpenURL = func(string) error { return nil }
	dependencies := Dependencies{
		ConfigStore:     configStore,
		CredentialStore: store,
		LastFMClient:    client,
		Input:           strings.NewReader("\n"),
	}

	var stdout, stderr bytes.Buffer
	if exitCode := RunWithDependencies([]string{"login"}, &stdout, &stderr, dependencies); exitCode != 0 {
		t.Fatalf("fallback login exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := fallback.sessions["personal"]; got != "session-1" {
		t.Fatalf("fallback credential = %q, want session-1", got)
	}

	native.loadErr = nil
	native.saveErr = nil
	native.deleteErr = nil
	stdout.Reset()
	stderr.Reset()
	if exitCode := RunWithDependencies([]string{"logout"}, &stdout, &stderr, dependencies); exitCode != 0 {
		t.Fatalf("logout exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if _, ok := fallback.sessions["personal"]; ok {
		t.Fatal("logout retained the dormant fallback credential")
	}

	stdout.Reset()
	stderr.Reset()
	dependencies.Input = strings.NewReader("\n")
	if exitCode := RunWithDependencies([]string{"login"}, &stdout, &stderr, dependencies); exitCode != 0 {
		t.Fatalf("native login exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	if got := native.sessions["personal"]; got != "session-2" {
		t.Fatalf("native credential = %q, want session-2", got)
	}
	if _, ok := fallback.sessions["personal"]; ok {
		t.Fatal("native login retained a fallback credential")
	}
}

func TestLinuxLogoutClearsDormantFallbackProfileAndPreservesOthers(t *testing.T) {
	configStore := config.NewFileStore(filepath.Join(t.TempDir(), "config.json"))
	if err := configStore.Save(config.Config{
		Profiles: map[string]config.Profile{
			"personal": {Name: "personal"},
			"family":   {Name: "family"},
		},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	native := &commandCredentialStore{sessions: map[string]string{
		"personal": "native-personal",
		"family":   "native-family",
	}}
	file := &commandCredentialStore{sessions: map[string]string{
		"personal": "dormant-personal",
		"family":   "dormant-family",
	}}

	var stdout, stderr bytes.Buffer
	if exitCode := RunWithDependencies(
		[]string{"--profile", "personal", "logout"},
		&stdout,
		&stderr,
		Dependencies{
			ConfigStore:     configStore,
			CredentialStore: credentials.NewPlatformStore("linux", native, file),
		},
	); exitCode != 0 {
		t.Fatalf("logout exit code = %d, stderr = %q", exitCode, stderr.String())
	}
	for tier, backend := range map[string]*commandCredentialStore{"native": native, "fallback": file} {
		if _, ok := backend.sessions["personal"]; ok {
			t.Errorf("%s store retained personal credential", tier)
		}
		if backend.sessions["family"] == "" {
			t.Errorf("%s store deleted family credential", tier)
		}
	}
}

func TestLoginReportsActionableUnavailableSecureStore(t *testing.T) {
	store := config.NewFileStore(filepath.Join(t.TempDir(), "config.json"))
	secureStoreErr := &credentials.UnavailableError{Cause: errors.New("Secret Service is unavailable")}
	credentialsStore := &commandCredentialStore{loadErr: secureStoreErr}
	var stdout, stderr bytes.Buffer
	exitCode := RunWithDependencies(
		[]string{"--profile", "personal", "login"},
		&stdout,
		&stderr,
		Dependencies{
			ConfigStore:     store,
			CredentialStore: credentialsStore,
			LastFMClient:    lastfm.NewClient("app-key", "app-secret"),
		},
	)
	if exitCode == 0 {
		t.Fatal("expected login to fail without a secure store")
	}
	if !strings.Contains(stderr.String(), "make an OS-native credential service available") {
		t.Fatalf("stderr = %q, want actionable secure-store guidance", stderr.String())
	}
}

func TestAuthenticatedCommandRequestUsesResolvedProfileCredentialAndSignature(t *testing.T) {
	const (
		apiKey     = "app-key"
		apiSecret  = "app-secret"
		sessionKey = "work-session"
	)
	store := config.NewFileStore(filepath.Join(t.TempDir(), "config.json"))
	if err := store.Save(config.Config{
		Profiles: map[string]config.Profile{
			"personal": {Name: "personal"},
			"work":     {Name: "work"},
		},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	credentialsStore := &commandCredentialStore{sessions: map[string]string{
		"personal": "personal-session",
		"work":     sessionKey,
	}}
	var received url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("request method = %s, want POST", r.Method)
		}
		if got := r.URL.Query().Get("sk"); got != "" {
			t.Errorf("session key leaked in URL query = %q", got)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
		}
		received = r.PostForm
		fmt.Fprint(w, `{}`)
	}))
	defer server.Close()
	client := lastfm.NewClient(apiKey, apiSecret)
	client.BaseURL = server.URL

	options, _, err := parseArgs([]string{"--profile", "work", "status"})
	if err != nil {
		t.Fatalf("parse profile selection: %v", err)
	}
	if _, err := callAuthenticated(
		context.Background(),
		options,
		"user.getInfo",
		nil,
		Dependencies{ConfigStore: store, CredentialStore: credentialsStore, LastFMClient: client},
	); err != nil {
		t.Fatalf("authenticated Last.fm call: %v", err)
	}

	if got := received.Get("sk"); got != sessionKey {
		t.Fatalf("session key = %q, want %q", got, sessionKey)
	}
	if got := received.Get("api_key"); got != apiKey {
		t.Fatalf("api key = %q, want %q", got, apiKey)
	}
	if got := received.Get("method"); got != "user.getInfo" {
		t.Fatalf("method = %q, want user.getInfo", got)
	}
	if got := received.Get("format"); got != "json" {
		t.Fatalf("format = %q, want json", got)
	}
	wantSignature := signedRequestSignature(map[string]string{
		"api_key": apiKey,
		"method":  "user.getInfo",
		"sk":      sessionKey,
	}, apiSecret)
	if got := received.Get("api_sig"); got != wantSignature {
		t.Fatalf("api signature = %q, want %q", got, wantSignature)
	}
}

func TestHistoryRequestUsesAuthenticatedClientEstablishedByLoginContract(t *testing.T) {
	const (
		apiKey     = "app-key"
		apiSecret  = "app-secret"
		sessionKey = "login-session"
	)
	start := time.Unix(1788307200, 0).UTC()
	end := time.Unix(1788307260, 0).UTC()
	var historyRequest url.Values
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			t.Errorf("request method = %s, want POST", r.Method)
		}
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		switch r.PostForm.Get("method") {
		case "auth.getToken":
			fmt.Fprint(w, `{"token":"login-token"}`)
		case "auth.getSession":
			if got := r.PostForm.Get("token"); got != "login-token" {
				t.Errorf("auth.getSession token = %q, want login-token", got)
			}
			fmt.Fprint(w, `{"session":{"name":"alice","key":"login-session"}}`)
		case "user.getRecentTracks":
			historyRequest = r.PostForm
			fmt.Fprint(w, `{"recenttracks":{"track":[{"artist":{"#text":"Artist"},"name":"Track","date":{"uts":"1788307200"}}],"@attr":{"totalPages":"1"}}}`)
		default:
			http.Error(w, "unexpected method", http.StatusBadRequest)
		}
	}))
	defer server.Close()

	store := config.NewFileStore(filepath.Join(t.TempDir(), "config.json"))
	credentialsStore := &commandCredentialStore{}
	client := lastfm.NewClient(apiKey, apiSecret)
	client.BaseURL = server.URL
	client.OpenURL = func(string) error { return nil }

	var stdout, stderr bytes.Buffer
	if exitCode := RunWithDependencies(
		[]string{"--profile", "personal", "login"},
		&stdout,
		&stderr,
		Dependencies{
			ConfigStore:     store,
			CredentialStore: credentialsStore,
			LastFMClient:    client,
			Input:           strings.NewReader("\n"),
		},
	); exitCode != 0 {
		t.Fatalf("login exit code = %d, stderr = %q", exitCode, stderr.String())
	}

	cfg, err := store.Load()
	if err != nil {
		t.Fatalf("load configuration after login: %v", err)
	}
	profile, err := cfg.ResolveProfile("personal")
	if err != nil {
		t.Fatalf("resolve logged-in profile: %v", err)
	}
	storedSession, err := credentialsStore.Load("personal")
	if err != nil {
		t.Fatalf("load logged-in session: %v", err)
	}

	var got []lastfm.Scrobble
	err = lastfm.ReadHistory(context.Background(), client, lastfm.AuthenticatedProfile{
		Username:   profile.LastFMUsername,
		SessionKey: storedSession,
	}, start, end, func(scrobble lastfm.Scrobble) error {
		got = append(got, scrobble)
		return nil
	})
	if err != nil {
		t.Fatalf("read authenticated history: %v", err)
	}
	if len(got) != 1 || got[0].Artist != "Artist" || got[0].Track != "Track" || !got[0].Timestamp.Equal(start) {
		t.Fatalf("history = %#v, want one scrobble at %s", got, start)
	}
	if got := historyRequest.Get("user"); got != "alice" {
		t.Fatalf("history username = %q, want alice", got)
	}
	if got := historyRequest.Get("sk"); got != sessionKey {
		t.Fatalf("history session = %q, want stored login session", got)
	}
	if got := historyRequest.Get("from"); got != fmt.Sprint(start.Unix()) {
		t.Fatalf("history from = %q, want %d", got, start.Unix())
	}
	if got := historyRequest.Get("to"); got != fmt.Sprint(end.Unix()) {
		t.Fatalf("history to = %q, want %d", got, end.Unix())
	}
	wantSignature := signedRequestSignature(map[string]string{
		"api_key": apiKey,
		"from":    fmt.Sprint(start.Unix()),
		"method":  "user.getRecentTracks",
		"page":    "1",
		"sk":      sessionKey,
		"to":      fmt.Sprint(end.Unix()),
		"user":    "alice",
	}, apiSecret)
	if got := historyRequest.Get("api_sig"); got != wantSignature {
		t.Fatalf("history API signature = %q, want %q", got, wantSignature)
	}
}

func signedRequestSignature(params map[string]string, secret string) string {
	keys := make([]string, 0, len(params))
	for key := range params {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	var input strings.Builder
	for _, key := range keys {
		input.WriteString(key)
		input.WriteString(params[key])
	}
	input.WriteString(secret)
	sum := md5.Sum([]byte(input.String()))
	return hex.EncodeToString(sum[:])
}
