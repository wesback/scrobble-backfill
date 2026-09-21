// Package credentials defines the secure credential boundary used by
// Rescrobble. Last.fm sessions are keyed by profile name and are never part of
// the persistent configuration.
package credentials

import (
	"errors"
	"fmt"
	"strings"

	"github.com/zalando/go-keyring"
)

const (
	// ServiceName is the service identifier used in the operating system
	// credential store.
	ServiceName = "rescrobble/lastfm"

	unavailableStoreGuidance = "make an OS-native credential service available (Windows Credential Manager, macOS Keychain, or Linux Secret Service)"
)

var (
	// ErrCredentialNotFound indicates that a profile has no saved session.
	ErrCredentialNotFound = errors.New("Last.fm session credential not found")

	// ErrSecureStoreUnavailable identifies a host where the OS credential
	// service cannot be used. There is intentionally no file-backed fallback.
	ErrSecureStoreUnavailable = errors.New("secure credential store unavailable")
)

// Store is the credential boundary for profile-scoped Last.fm sessions.
//
// Implementations must not persist credentials in the application
// configuration or in a plaintext file.
type Store interface {
	Save(profile, session string) error
	Load(profile string) (string, error)
	Delete(profile string) error
	Has(profile string) (bool, error)
}

// CredentialStore is an explicit name for Store for callers that prefer the
// domain terminology.
type CredentialStore = Store

// OSStore stores sessions in the operating system's native credential
// service. The underlying library selects Windows Credential Manager, macOS
// Keychain, or Linux Secret Service according to the host platform.
type OSStore struct {
	backend keyringBackend
}

type keyringBackend interface {
	Set(service, user, password string) error
	Get(service, user string) (string, error)
	Delete(service, user string) error
}

type nativeBackend struct{}

func (nativeBackend) Set(service, user, password string) error {
	return keyring.Set(service, user, password)
}

func (nativeBackend) Get(service, user string) (string, error) {
	return keyring.Get(service, user)
}

func (nativeBackend) Delete(service, user string) error {
	return keyring.Delete(service, user)
}

// NewOSStore creates a credential store backed exclusively by the host's
// native secure credential service.
func NewOSStore() *OSStore {
	return &OSStore{backend: nativeBackend{}}
}

// NewStore creates the production OS-native credential store.
func NewStore() *OSStore {
	return NewOSStore()
}

// NewDefaultStore creates the production OS-native credential store.
func NewDefaultStore() *OSStore {
	return NewOSStore()
}

// Save stores session for profile, replacing any existing session.
func (s *OSStore) Save(profile, session string) error {
	if err := validateProfile(profile); err != nil {
		return err
	}
	if strings.TrimSpace(session) == "" {
		return errors.New("Last.fm session credential must not be empty")
	}
	if s == nil || s.backend == nil {
		return unavailableError(nil)
	}
	if err := s.backend.Set(ServiceName, profile, session); err != nil {
		return unavailableError(err)
	}
	return nil
}

// Load retrieves session for profile.
func (s *OSStore) Load(profile string) (string, error) {
	if err := validateProfile(profile); err != nil {
		return "", err
	}
	if s == nil || s.backend == nil {
		return "", unavailableError(nil)
	}
	session, err := s.backend.Get(ServiceName, profile)
	if errors.Is(err, keyring.ErrNotFound) {
		return "", ErrCredentialNotFound
	}
	if err != nil {
		return "", unavailableError(err)
	}
	return session, nil
}

// Delete removes the session for profile. Deleting an already absent session
// succeeds, which makes logout safe to retry.
func (s *OSStore) Delete(profile string) error {
	if err := validateProfile(profile); err != nil {
		return err
	}
	if s == nil || s.backend == nil {
		return unavailableError(nil)
	}
	if err := s.backend.Delete(ServiceName, profile); err != nil {
		if errors.Is(err, keyring.ErrNotFound) {
			return nil
		}
		return unavailableError(err)
	}
	return nil
}

// Has reports whether profile has a saved session.
func (s *OSStore) Has(profile string) (bool, error) {
	if err := validateProfile(profile); err != nil {
		return false, err
	}
	if s == nil || s.backend == nil {
		return false, unavailableError(nil)
	}
	_, err := s.backend.Get(ServiceName, profile)
	if errors.Is(err, keyring.ErrNotFound) {
		return false, nil
	}
	if err != nil {
		return false, unavailableError(err)
	}
	return true, nil
}

// UnavailableError reports that the secure operating system credential
// service could not be used. It deliberately contains actionable setup
// guidance and does not suggest a less secure fallback.
type UnavailableError struct {
	Cause error
}

func (e *UnavailableError) Error() string {
	if e == nil || e.Cause == nil {
		return fmt.Sprintf("%s; %s", ErrSecureStoreUnavailable, unavailableStoreGuidance)
	}
	return fmt.Sprintf("%s: %v; %s", ErrSecureStoreUnavailable, e.Cause, unavailableStoreGuidance)
}

func (e *UnavailableError) Unwrap() error {
	if e == nil {
		return nil
	}
	return e.Cause
}

func (e *UnavailableError) Is(target error) bool {
	return target == ErrSecureStoreUnavailable
}

func unavailableError(cause error) error {
	return &UnavailableError{Cause: cause}
}

func validateProfile(profile string) error {
	if strings.TrimSpace(profile) == "" {
		return errors.New("profile name must not be empty")
	}
	if profile != strings.TrimSpace(profile) {
		return errors.New("profile name must not start or end with whitespace")
	}
	return nil
}
