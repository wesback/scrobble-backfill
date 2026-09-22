package cli

import (
	"bytes"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesback/scrobble-backfill/internal/config"
	"github.com/wesback/scrobble-backfill/internal/credentials"
	"github.com/wesback/scrobble-backfill/internal/journal"
	"github.com/wesback/scrobble-backfill/internal/lastfm"
)

func TestDoctorHealthyProfileSelectionIsReadOnlyAndDoesNotSubmit(t *testing.T) {
	root := t.TempDir()
	configPath := filepath.Join(root, "config.json")
	configStore := config.NewFileStore(configPath)
	if err := configStore.Save(config.Config{
		Profiles: map[string]config.Profile{
			"personal": {Name: "personal", LastFMUsername: "personal-user"},
			"work":     {Name: "work", LastFMUsername: "work-user"},
		},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	journalStore := journal.NewFileStore(filepath.Join(root, "journal"))
	if _, err := journalStore.CreateRun("work", "existing-run"); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	credentialsStore := &commandCredentialStore{sessions: map[string]string{
		"personal": "personal-session-secret",
		"work":     "work-session-secret",
	}}
	var methods []string
	var users []string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		methods = append(methods, r.PostForm.Get("method"))
		users = append(users, r.PostForm.Get("user"))
		if r.PostForm.Get("method") == "track.scrobble" {
			http.Error(w, "submission is not allowed", http.StatusBadRequest)
			return
		}
		fmt.Fprint(w, `{"user":{"name":"work-user"}}`)
	}))
	defer server.Close()
	client := lastfm.NewClient("app-key", "app-secret")
	client.BaseURL = server.URL

	beforeConfig := readFile(t, configPath)
	beforeJournal := snapshotFiles(t, filepath.Join(root, "journal"))
	beforeCredentials := cloneStrings(credentialsStore.sessions)

	var stdout, stderr bytes.Buffer
	exitCode := RunWithDependencies(
		[]string{"--profile", "work", "doctor"},
		&stdout,
		&stderr,
		Dependencies{
			ConfigStore:     configStore,
			CredentialStore: credentialsStore,
			LastFMClient:    client,
			JournalStore:    journalStore,
		},
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q, stdout = %q", exitCode, stderr.String(), stdout.String())
	}
	for _, check := range []string{
		"PASS: configuration readability",
		"PASS: secure keyring availability",
		"PASS: credential presence and validity",
		"PASS: Last.fm API reachability and authentication",
		"PASS: journal readability",
	} {
		if !strings.Contains(stdout.String(), check) {
			t.Fatalf("stdout = %q, want %q", stdout.String(), check)
		}
	}
	if !strings.Contains(stdout.String(), `Profile: "work"`) {
		t.Fatalf("stdout = %q, want explicit profile selection", stdout.String())
	}
	if len(methods) != 1 || methods[0] != "user.getInfo" {
		t.Fatalf("Last.fm methods = %#v, want only user.getInfo", methods)
	}
	if len(users) != 1 || users[0] != "work-user" {
		t.Fatalf("Last.fm users = %#v, want work-user", users)
	}
	if strings.Contains(stdout.String(), "session-secret") || strings.Contains(stderr.String(), "session-secret") {
		t.Fatalf("diagnostic output exposes a session credential: stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	if got := readFile(t, configPath); !bytes.Equal(got, beforeConfig) {
		t.Fatalf("doctor changed configuration: before=%q after=%q", beforeConfig, got)
	}
	if got := snapshotFiles(t, filepath.Join(root, "journal")); !mapsEqual(got, beforeJournal) {
		t.Fatalf("doctor changed journal: before=%v after=%v", beforeJournal, got)
	}
	if !stringMapsEqual(credentialsStore.sessions, beforeCredentials) {
		t.Fatalf("doctor changed credentials: before=%v after=%v", beforeCredentials, credentialsStore.sessions)
	}
}

func TestDoctorUsesActiveProfileWhenNoExplicitProfileIsProvided(t *testing.T) {
	root := t.TempDir()
	configStore := config.NewFileStore(filepath.Join(root, "config.json"))
	if err := configStore.Save(config.Config{
		Profiles: map[string]config.Profile{
			"personal": {Name: "personal", LastFMUsername: "personal-user"},
			"work":     {Name: "work", LastFMUsername: "work-user"},
		},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	var requestedUser string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := r.ParseForm(); err != nil {
			t.Errorf("parse form: %v", err)
			return
		}
		requestedUser = r.PostForm.Get("user")
		fmt.Fprint(w, `{"user":{"name":"personal-user"}}`)
	}))
	defer server.Close()
	client := lastfm.NewClient("app-key", "app-secret")
	client.BaseURL = server.URL

	var stdout, stderr bytes.Buffer
	exitCode := RunWithDependencies(
		[]string{"doctor"},
		&stdout,
		&stderr,
		Dependencies{
			ConfigStore:     configStore,
			CredentialStore: &commandCredentialStore{sessions: map[string]string{"personal": "personal-session"}},
			LastFMClient:    client,
			JournalStore:    journal.NewFileStore(filepath.Join(root, "journal")),
		},
	)
	if exitCode != 0 {
		t.Fatalf("exit code = %d, stderr = %q, stdout = %q", exitCode, stderr.String(), stdout.String())
	}
	if !strings.Contains(stdout.String(), `Profile: "personal"`) || requestedUser != "personal-user" {
		t.Fatalf("stdout = %q, requested user = %q; want active personal profile", stdout.String(), requestedUser)
	}
}

func TestDoctorReportsEachFailedDiagnostic(t *testing.T) {
	tests := []struct {
		name       string
		prepare    func(t *testing.T, root string) (config.Store, credentials.Store, *lastfm.Client, journal.Store)
		wantChecks []string
	}{
		{
			name: "secure keyring unavailable",
			prepare: func(t *testing.T, root string) (config.Store, credentials.Store, *lastfm.Client, journal.Store) {
				configStore, journalStore := healthyDoctorStores(t, root)
				return configStore,
					&commandCredentialStore{loadErr: &credentials.UnavailableError{Cause: errors.New("Secret Service unavailable")}},
					healthyDoctorClient(t, `{"user":{"name":"alice"}}`),
					journalStore
			},
			wantChecks: []string{"FAIL: secure keyring availability", "secure credential store unavailable"},
		},
		{
			name: "credential absent",
			prepare: func(t *testing.T, root string) (config.Store, credentials.Store, *lastfm.Client, journal.Store) {
				configStore, journalStore := healthyDoctorStores(t, root)
				return configStore, &commandCredentialStore{}, healthyDoctorClient(t, `{"user":{"name":"alice"}}`), journalStore
			},
			wantChecks: []string{"FAIL: credential presence and validity", "no stored credential; run login"},
		},
		{
			name: "credential rejected",
			prepare: func(t *testing.T, root string) (config.Store, credentials.Store, *lastfm.Client, journal.Store) {
				configStore, journalStore := healthyDoctorStores(t, root)
				return configStore,
					&commandCredentialStore{sessions: map[string]string{"personal": "invalid-session-secret"}},
					healthyDoctorClient(t, `{"error":9,"message":"Invalid session key"}`),
					journalStore
			},
			wantChecks: []string{"FAIL: credential presence and validity", "stored credential was rejected", "FAIL: Last.fm API reachability and authentication"},
		},
		{
			name: "api unreachable",
			prepare: func(t *testing.T, root string) (config.Store, credentials.Store, *lastfm.Client, journal.Store) {
				configStore, journalStore := healthyDoctorStores(t, root)
				server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
				baseURL := server.URL
				server.Close()
				client := lastfm.NewClient("app-key", "app-secret")
				client.BaseURL = baseURL
				return configStore,
					&commandCredentialStore{sessions: map[string]string{"personal": "session-secret"}},
					client,
					journalStore
			},
			wantChecks: []string{"FAIL: Last.fm API reachability and authentication", "unreachable or not configured"},
		},
		{
			name: "api server error",
			prepare: func(t *testing.T, root string) (config.Store, credentials.Store, *lastfm.Client, journal.Store) {
				configStore, journalStore := healthyDoctorStores(t, root)
				return configStore,
					&commandCredentialStore{sessions: map[string]string{"personal": "session-secret"}},
					healthyDoctorClientWithStatus(t, http.StatusBadGateway, "upstream unavailable"),
					journalStore
			},
			wantChecks: []string{"FAIL: Last.fm API reachability and authentication", "Last.fm API is unreachable or not configured"},
		},
		{
			name: "api malformed response",
			prepare: func(t *testing.T, root string) (config.Store, credentials.Store, *lastfm.Client, journal.Store) {
				configStore, journalStore := healthyDoctorStores(t, root)
				return configStore,
					&commandCredentialStore{sessions: map[string]string{"personal": "session-secret"}},
					healthyDoctorClient(t, "not-json"),
					journalStore
			},
			wantChecks: []string{"FAIL: Last.fm API reachability and authentication", "Last.fm API request failed"},
		},
		{
			name: "application credentials rejected",
			prepare: func(t *testing.T, root string) (config.Store, credentials.Store, *lastfm.Client, journal.Store) {
				configStore, journalStore := healthyDoctorStores(t, root)
				return configStore,
					&commandCredentialStore{sessions: map[string]string{"personal": "session-secret"}},
					healthyDoctorClient(t, `{"error":10,"message":"Invalid API key"}`),
					journalStore
			},
			wantChecks: []string{"FAIL: Last.fm API reachability and authentication", "Last.fm API returned an error"},
		},
		{
			name: "configuration unreadable",
			prepare: func(t *testing.T, root string) (config.Store, credentials.Store, *lastfm.Client, journal.Store) {
				configPath := filepath.Join(root, "config.json")
				if err := os.WriteFile(configPath, []byte("{not-json"), 0o600); err != nil {
					t.Fatalf("write invalid config: %v", err)
				}
				return config.NewFileStore(configPath), &commandCredentialStore{}, nil, journal.NewFileStore(filepath.Join(root, "journal"))
			},
			wantChecks: []string{"FAIL: configuration readability", "configuration is unreadable"},
		},
		{
			name: "journal unreadable",
			prepare: func(t *testing.T, root string) (config.Store, credentials.Store, *lastfm.Client, journal.Store) {
				configStore, _ := healthyDoctorStores(t, root)
				journalPath := filepath.Join(root, "journal")
				if err := os.WriteFile(journalPath, []byte("not a directory"), 0o600); err != nil {
					t.Fatalf("write journal blocker: %v", err)
				}
				return configStore,
					&commandCredentialStore{sessions: map[string]string{"personal": "session-secret"}},
					healthyDoctorClient(t, `{"user":{"name":"alice"}}`),
					journal.NewFileStore(journalPath)
			},
			wantChecks: []string{"FAIL: journal readability", "journal is unreadable"},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			configStore, credentialStore, client, journalStore := test.prepare(t, root)
			var stdout, stderr bytes.Buffer
			exitCode := RunWithDependencies(
				[]string{"doctor"},
				&stdout,
				&stderr,
				Dependencies{
					ConfigStore:     configStore,
					CredentialStore: credentialStore,
					LastFMClient:    client,
					JournalStore:    journalStore,
				},
			)
			if exitCode == 0 {
				t.Fatalf("doctor unexpectedly succeeded; stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
			for _, want := range test.wantChecks {
				if !strings.Contains(stdout.String(), want) {
					t.Fatalf("stdout = %q, want %q", stdout.String(), want)
				}
			}
			if strings.Contains(stdout.String(), "session-secret") || strings.Contains(stderr.String(), "session-secret") {
				t.Fatalf("diagnostic output exposes a session credential: stdout=%q stderr=%q", stdout.String(), stderr.String())
			}
		})
	}
}

func healthyDoctorStores(t *testing.T, root string) (config.Store, journal.Store) {
	t.Helper()
	configStore := config.NewFileStore(filepath.Join(root, "config.json"))
	if err := configStore.Save(config.Config{
		Profiles:      map[string]config.Profile{"personal": {Name: "personal", LastFMUsername: "alice"}},
		ActiveProfile: "personal",
	}); err != nil {
		t.Fatalf("seed config: %v", err)
	}
	journalStore := journal.NewFileStore(filepath.Join(root, "journal"))
	return configStore, journalStore
}

func healthyDoctorClient(t *testing.T, response string) *lastfm.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, response)
	}))
	t.Cleanup(server.Close)
	client := lastfm.NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	return client
}

func healthyDoctorClientWithStatus(t *testing.T, status int, response string) *lastfm.Client {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, response, status)
	}))
	t.Cleanup(server.Close)
	client := lastfm.NewClient("app-key", "app-secret")
	client.BaseURL = server.URL
	return client
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return data
}

func snapshotFiles(t *testing.T, root string) map[string][]byte {
	t.Helper()
	snapshot := make(map[string][]byte)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.IsDir() {
			return nil
		}
		snapshot[path] = readFile(t, path)
		return nil
	})
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("snapshot %s: %v", root, err)
	}
	return snapshot
}

func cloneStrings(values map[string]string) map[string]string {
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func mapsEqual(left, right map[string][]byte) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if !bytes.Equal(value, right[key]) {
			return false
		}
	}
	return true
}

func stringMapsEqual(left, right map[string]string) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}
