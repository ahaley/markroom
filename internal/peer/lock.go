package peer

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// staleLockAge is how long a lock file is honoured before it is assumed to
// belong to a process that died without cleaning up.
const staleLockAge = 15 * time.Minute

// ErrLocked reports that another markroom process is already syncing.
var ErrLocked = errors.New("another markroom sync is already running")

// Lock takes an advisory lock over the cache directory and returns a release
// function. index.db is protected by SQLite's own locking, but the cache and
// config.json are not, so two syncs running at once would fight over both.
func Lock(cacheDir string) (func(), error) {
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, err
	}
	path := filepath.Join(cacheDir, ".sync.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	if errors.Is(err, os.ErrExist) {
		if !stale(path) {
			return nil, ErrLocked
		}
		os.Remove(path)
		f, err = os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o600)
	}
	if err != nil {
		return nil, err
	}
	fmt.Fprintf(f, "%d %d\n", os.Getpid(), time.Now().Unix())
	f.Close()
	return func() { os.Remove(path) }, nil
}

func stale(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return true
	}
	started, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil {
		return true
	}
	return time.Since(time.Unix(started, 0)) > staleLockAge
}
