//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris && !windows

package journal

import "errors"

func acquireProfileFileLock(string) (profileLock, error) {
	return nil, errors.New("journal file locking is unsupported on this platform")
}
