//go:build unix

package state

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
)

// lockFile is an flock on a file beside the state file. The kernel drops it
// when the process exits, however it exits. It sits beside the database
// rather than on it so it can't interfere with SQLite's own locks.
type lockFile struct{ f *os.File }

func acquireLock(path string) (*lockFile, error) {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("state: opening the lock file: %w", err)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		b, _ := os.ReadFile(path)
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			if pid := strings.TrimSpace(string(b)); pid != "" {
				return nil, fmt.Errorf("%w (pid %s holds %s)", ErrLocked, pid, path)
			}
			return nil, fmt.Errorf("%w (%s is held)", ErrLocked, path)
		}
		return nil, fmt.Errorf("state: locking %s: %w", path, err)
	}
	// The pid is only for the message a second process prints.
	f.Truncate(0)
	f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
	return &lockFile{f: f}, nil
}

func (l *lockFile) release() {
	l.f.Truncate(0)
	l.f.Close()
}
