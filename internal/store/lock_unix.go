//go:build unix

package store

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

const (
	lockFile         = "lock"
	lockPollInterval = 25 * time.Millisecond
)

// Lock takes the data directory's exclusive lock. It waits until the lock is
// free or ctx ends. Call the returned function to release it.
//
// The lock is flock(2) on a separate file, so it works across processes that
// share the data directory, including two containers on one volume.
func (s *Store) Lock(ctx context.Context) (func(), error) {
	f, err := os.OpenFile(filepath.Join(s.dir, lockFile), os.O_RDWR|os.O_CREATE, fileMode)
	if err != nil {
		return nil, fmt.Errorf("open lock file: %w", err)
	}
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
				f.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, fmt.Errorf("lock data directory: %w", err)
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, fmt.Errorf("wait for data directory lock: %w", ctx.Err())
		case <-time.After(lockPollInterval):
		}
	}
}
