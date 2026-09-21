// Package config owns the persistent, non-secret configuration used by the
// command line application. Credentials are deliberately not represented by
// any type in this package.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	// ConfigPathEnv allows an installation or test to select a configuration
	// file without changing the user's normal configuration location.
	ConfigPathEnv  = "RESCOBBLE_CONFIG"
	configDirName  = "rescrobble"
	configFileName = "config.json"
)

// Profile contains identity and other non-secret metadata for a named
// account. Authentication credentials are stored by the credential boundary,
// not in this configuration.
type Profile struct {
	Name           string `json:"name,omitempty"`
	DisplayName    string `json:"display_name,omitempty"`
	LastFMUsername string `json:"lastfm_username,omitempty"`
}

// Config is the on-disk configuration document.
type Config struct {
	Profiles      map[string]Profile `json:"profiles"`
	ActiveProfile string             `json:"active_profile,omitempty"`
}

// Store is the persistence boundary used by commands. Implementations must
// preserve profiles and the active profile across Load and Save calls.
type Store interface {
	Load() (Config, error)
	Save(Config) error
}

// FileStore persists Config as JSON at Path.
type FileStore struct {
	Path string
}

// NewFileStore creates a file-backed configuration store.
func NewFileStore(path string) *FileStore {
	return &FileStore{Path: path}
}

// NewStore is an alias for NewFileStore, kept as the concise constructor for
// callers that do not need to name the backing format.
func NewStore(path string) *FileStore {
	return NewFileStore(path)
}

// DefaultPath returns the platform-specific configuration path, or the path
// selected by RESCOBBLE_CONFIG when it is set.
func DefaultPath() (string, error) {
	if path := strings.TrimSpace(os.Getenv(ConfigPathEnv)); path != "" {
		return path, nil
	}

	configDir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("determine configuration directory: %w", err)
	}
	return filepath.Join(configDir, configDirName, configFileName), nil
}

// NewDefaultStore creates a store at DefaultPath.
func NewDefaultStore() (*FileStore, error) {
	path, err := DefaultPath()
	if err != nil {
		return nil, err
	}
	return NewFileStore(path), nil
}

// Load reads the configuration. A missing file is an empty configuration,
// which allows the first profile to be created by a later authentication
// command without requiring a bootstrap file.
func (s *FileStore) Load() (Config, error) {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return Config{}, errors.New("configuration path is empty")
	}

	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		return NewConfig(), nil
	}
	if err != nil {
		return Config{}, fmt.Errorf("read configuration %q: %w", s.Path, err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse configuration %q: %w", s.Path, err)
	}
	if cfg.Profiles == nil {
		cfg.Profiles = make(map[string]Profile)
	}
	if err := cfg.Validate(); err != nil {
		return Config{}, fmt.Errorf("validate configuration %q: %w", s.Path, err)
	}
	return cfg, nil
}

// Save validates and atomically writes the configuration with owner-only
// permissions. The document contains no credentials, but restrictive
// permissions avoid exposing account metadata unnecessarily.
func (s *FileStore) Save(cfg Config) error {
	if s == nil || strings.TrimSpace(s.Path) == "" {
		return errors.New("configuration path is empty")
	}
	if err := cfg.Validate(); err != nil {
		return fmt.Errorf("validate configuration: %w", err)
	}

	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("encode configuration: %w", err)
	}
	data = append(data, '\n')

	dir := filepath.Dir(s.Path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create configuration directory %q: %w", dir, err)
	}

	temp, err := os.CreateTemp(dir, "."+filepath.Base(s.Path)+".tmp-*")
	if err != nil {
		return fmt.Errorf("create temporary configuration file: %w", err)
	}
	tempName := temp.Name()
	cleanup := func() {
		_ = temp.Close()
		_ = os.Remove(tempName)
	}
	defer cleanup()

	if err := temp.Chmod(0o600); err != nil {
		return fmt.Errorf("set configuration permissions: %w", err)
	}
	if _, err := io.Copy(temp, bytes.NewReader(data)); err != nil {
		return fmt.Errorf("write configuration: %w", err)
	}
	if err := temp.Sync(); err != nil {
		return fmt.Errorf("sync configuration: %w", err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("close configuration: %w", err)
	}
	if err := os.Rename(tempName, s.Path); err != nil {
		return fmt.Errorf("replace configuration %q: %w", s.Path, err)
	}
	return nil
}

// NewConfig returns an initialized empty configuration.
func NewConfig() Config {
	return Config{Profiles: make(map[string]Profile)}
}

// Validate checks profile names and the active-profile reference. It does not
// inspect or accept credentials.
func (c Config) Validate() error {
	for name := range c.Profiles {
		if err := validateName(name); err != nil {
			return fmt.Errorf("profile %q: %w", name, err)
		}
	}
	if c.ActiveProfile != "" && !c.HasProfile(c.ActiveProfile) {
		return fmt.Errorf("active profile %q is not configured", c.ActiveProfile)
	}
	return nil
}

// HasProfile reports whether name is configured.
func (c Config) HasProfile(name string) bool {
	_, ok := c.Profiles[name]
	return ok
}

// ProfileNames returns configured profile names in stable order.
func (c Config) ProfileNames() []string {
	names := make([]string, 0, len(c.Profiles))
	for name := range c.Profiles {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// ResolveProfileName applies the selection contract shared by commands:
// explicit takes precedence over the persisted active profile, and either
// selection must refer to a configured profile.
func (c Config) ResolveProfileName(explicit string) (string, error) {
	name := strings.TrimSpace(explicit)
	if name == "" {
		name = c.ActiveProfile
	}
	if name == "" {
		return "", errors.New("no profile selected; pass --profile <name> or select one with \"profile use <name>\"")
	}
	if !c.HasProfile(name) {
		return "", fmt.Errorf("profile %q is not configured; choose a configured profile or configure it before retrying", name)
	}
	return name, nil
}

// ResolveProfile applies the selection contract and returns the selected
// profile metadata.
func (c Config) ResolveProfile(explicit string) (Profile, error) {
	name, err := c.ResolveProfileName(explicit)
	if err != nil {
		return Profile{}, err
	}
	profile := c.Profiles[name]
	if profile.Name == "" {
		profile.Name = name
	}
	return profile, nil
}

// SetActiveProfile selects an existing configured profile.
func (c *Config) SetActiveProfile(name string) error {
	if c == nil {
		return errors.New("configuration is nil")
	}
	if err := validateName(name); err != nil {
		return fmt.Errorf("invalid profile name: %w", err)
	}
	if !c.HasProfile(name) {
		return fmt.Errorf("profile %q is not configured; choose a configured profile or configure it before retrying", name)
	}
	if c.Profiles == nil {
		c.Profiles = make(map[string]Profile)
	}
	c.ActiveProfile = name
	return nil
}

func validateName(name string) error {
	if strings.TrimSpace(name) == "" {
		return errors.New("profile name must not be empty")
	}
	if name != strings.TrimSpace(name) {
		return errors.New("profile name must not start or end with whitespace")
	}
	return nil
}
