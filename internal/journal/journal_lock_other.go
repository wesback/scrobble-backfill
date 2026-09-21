//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package journal

import "errors"

type profileLock struct{}

func acquireProfileFileLock(string) (*profileLock, error) {
	return nil, errors.New("journal file locking is unsupported on this platform")
}

func (*profileLock) Close() error {
	return nil
}
