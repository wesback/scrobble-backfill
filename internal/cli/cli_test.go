package cli

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"

	"github.com/wesback/scrobble-backfill/internal/config"
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
