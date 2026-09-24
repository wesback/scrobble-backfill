//go:build linux

package credentials

import "fmt"

// NewDefaultStore creates the production Linux credential store, which uses
// the native store first and the encrypted file store only when native access
// is unavailable.
func NewDefaultStore() (Store, error) {
	fallback, err := NewDefaultLinuxFileStore()
	var fallbackStore Store
	if err != nil {
		fallbackStore = &linuxFallbackInitializationError{err: fmt.Errorf("initialize encrypted credential fallback: %w", err)}
	} else {
		fallbackStore = fallback
	}
	return NewPlatformStore("linux", NewOSStore(), fallbackStore), nil
}

type linuxFallbackInitializationError struct {
	err error
}

func (s *linuxFallbackInitializationError) Save(string, string) error {
	return s.err
}

func (s *linuxFallbackInitializationError) Load(string) (string, error) {
	return "", s.err
}

func (s *linuxFallbackInitializationError) Delete(string) error {
	return s.err
}

func (s *linuxFallbackInitializationError) Has(string) (bool, error) {
	return false, s.err
}
