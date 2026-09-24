//go:build !linux

package credentials

// NewDefaultStore creates the production OS-native credential store.
func NewDefaultStore() (Store, error) {
	return NewOSStore(), nil
}
