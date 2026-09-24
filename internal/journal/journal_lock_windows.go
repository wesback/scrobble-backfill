//go:build windows

package journal

import (
	"errors"
	"os"

	"golang.org/x/sys/windows"
)

type windowsProfileLock struct {
	file       *os.File
	overlapped windows.Overlapped
}

func acquireProfileFileLock(path string) (profileLock, error) {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	lock := &windowsProfileLock{file: file}
	if err := windows.LockFileEx(windows.Handle(file.Fd()), windows.LOCKFILE_EXCLUSIVE_LOCK, 0, 1, 0, &lock.overlapped); err != nil {
		_ = file.Close()
		return nil, err
	}
	return lock, nil
}

func (lock *windowsProfileLock) Close() error {
	if lock == nil || lock.file == nil {
		return nil
	}
	file := lock.file
	lock.file = nil
	unlockErr := windows.UnlockFileEx(windows.Handle(file.Fd()), 0, 1, 0, &lock.overlapped)
	closeErr := file.Close()
	return errors.Join(unlockErr, closeErr)
}
