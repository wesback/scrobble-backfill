//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package journal

import (
	"errors"
	"os"

	"golang.org/x/sys/unix"
)

type unixProfileLock struct {
	file *os.File
}

func acquireProfileFileLock(path string) (profileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		_ = file.Close()
		return nil, err
	}
	return &unixProfileLock{file: file}, nil
}

func (lock *unixProfileLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	unlockErr := unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	closeErr := lock.file.Close()
	return errors.Join(unlockErr, closeErr)
}
