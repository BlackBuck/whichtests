// Package fsutil canonicalizes file paths so the two halves of the analysis
// agree on what a file is called.
//
// This exists because of a mismatch that is otherwise invisible: `git
// rev-parse --show-toplevel` reports a symlink-resolved path, while
// go/packages reports paths built from the directory it was handed. On macOS
// any repo under /tmp or /var (including every os.MkdirTemp directory) comes
// back as /private/var/... from git and /var/... from go/packages, so no diff
// hunk ever matches a function span and every change falls through to the
// conservative path. Same failure for anyone whose checkout lives behind a
// symlinked workspace directory.
package fsutil

import (
	"path/filepath"
	"sync"
)

var (
	mu       sync.Mutex
	dirCache = make(map[string]string)
)

// Canon returns an absolute, symlink-resolved form of p. Directory resolutions
// are cached, since a package's files all share a directory. A path that
// cannot be resolved (a deleted file, say) degrades to its absolute form
// rather than failing.
func Canon(p string) string {
	if p == "" {
		return p
	}
	dir, file := filepath.Split(p)
	if dir == "" {
		return p
	}
	return filepath.Join(canonDir(filepath.Clean(dir)), file)
}

func canonDir(dir string) string {
	mu.Lock()
	defer mu.Unlock()
	if v, ok := dirCache[dir]; ok {
		return v
	}
	out := dir
	if abs, err := filepath.Abs(dir); err == nil {
		out = abs
	}
	if res, err := filepath.EvalSymlinks(out); err == nil {
		out = res
	}
	dirCache[dir] = out
	dirCache[out] = out
	return out
}
