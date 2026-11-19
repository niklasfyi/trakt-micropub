//go:build !unix

package state

import "errors"

type lockFile struct{}

func acquireLock(string) (*lockFile, error) {
	return nil, errors.New("state: locking the state file needs a Unix system")
}

func (*lockFile) release() {}
