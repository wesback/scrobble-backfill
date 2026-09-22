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
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/wesback/scrobble-backfill/internal/config"
	"github.com/wesback/scrobble-backfill/internal/credentials"
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

func TestAnalyseUsesDefaultTimestampTolerance(t *testing.T) {
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
		Profiles:      map[string]config.Profile{"personal": {Name: "personal", LastFMUsername: "alice"}},
		ActiveProfile: "personal",
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
	if !strings.Contains(stdout.String(), "Timestamp tolerance: 1m0s") {
		t.Fatalf("stdout = %q, want default tolerance", stdout.String())
	}
	if len(methods) != 1 || methods[0] != "user.getRecentTracks" {
		t.Fatalf("Last.fm methods = %#v, want history only", methods)
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
		return nil
	}
	var stdout, stderr bytes.Buffer
	exitCode := RunWithDependencies(
		[]string{"--profile", "personal", "login"},
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
	configData, err := os.ReadFile(store.Path)
	if err != nil {
		t.Fatalf("read persisted config: %v", err)
	}
	if strings.Contains(string(configData), "session-secret") || strings.Contains(string(configData), "app-secret") || strings.Contains(string(configData), "app-key") {
		t.Fatalf("persisted config exposes a credential: %q", configData)
	}
	if strings.Contains(stdout.String(), "app-key") || strings.Contains(stdout.String(), "app-secret") || strings.Contains(stdout.String(), "session-secret") {
		t.Fatalf("stdout exposes a credential: %q", stdout.String())
	}
	if strings.Contains(stdout.String(), "login-token") {
		t.Fatalf("stdout exposes the authorization token: %q", stdout.String())
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
