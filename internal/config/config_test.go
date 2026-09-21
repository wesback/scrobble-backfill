package config

import (
	"path/filepath"
	"strings"
	"testing"
)

func TestFileStorePersistsAcrossReload(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.json")
	store := NewFileStore(path)
	initial := Config{
		Profiles: map[string]Profile{
			"personal": {Name: "personal", DisplayName: "Personal", LastFMUsername: "alice"},
			"family":   {Name: "family", DisplayName: "Family", LastFMUsername: "family-account"},
		},
		ActiveProfile: "personal",
	}

	if err := store.Save(initial); err != nil {
		t.Fatalf("save config: %v", err)
	}
	reloaded, err := NewFileStore(path).Load()
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if reloaded.ActiveProfile != initial.ActiveProfile {
		t.Fatalf("active profile = %q, want %q", reloaded.ActiveProfile, initial.ActiveProfile)
	}
	if got := reloaded.Profiles["family"].LastFMUsername; got != "family-account" {
		t.Fatalf("family username = %q, want family-account", got)
	}
}

func TestResolveProfileExplicitPrecedence(t *testing.T) {
	cfg := Config{
		Profiles: map[string]Profile{
			"personal": {Name: "personal"},
			"work":     {Name: "work"},
		},
		ActiveProfile: "personal",
	}

	name, err := cfg.ResolveProfileName("work")
	if err != nil {
		t.Fatalf("resolve explicit profile: %v", err)
	}
	if name != "work" {
		t.Fatalf("resolved profile = %q, want work", name)
	}

	name, err = cfg.ResolveProfileName("")
	if err != nil {
		t.Fatalf("resolve active profile: %v", err)
	}
	if name != "personal" {
		t.Fatalf("resolved active profile = %q, want personal", name)
	}
}

func TestResolveProfileRequiresSelection(t *testing.T) {
	cfg := NewConfig()
	_, err := cfg.ResolveProfileName("")
	if err == nil {
		t.Fatal("expected an error when no profile is available")
	}
	if !strings.Contains(err.Error(), `--profile <name>`) || !strings.Contains(err.Error(), `profile use <name>`) {
		t.Fatalf("error = %q, want guidance for --profile and profile use", err)
	}
}
