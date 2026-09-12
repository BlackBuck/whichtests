// Package gitdiff turns a git revision range into line ranges on the new side
// of the diff.
package gitdiff

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/BlackBuck/whichtests/internal/fsutil"
)

// Hunk is a contiguous changed line range in a file, expressed in new-side
// (working tree) line numbers.
type Hunk struct {
	File      string // absolute path
	StartLine int
	EndLine   int
	// Deletion marks a hunk that removed lines without adding any. The new-side
	// line number is then only an anchor, not a real changed line.
	Deletion bool
}

// Changed returns the hunks between the merge base of base..HEAD and the
// current working tree, so uncommitted edits count as changes too.
func Changed(dir, base string) ([]Hunk, error) {
	mergeBase, err := run(dir, "merge-base", base, "HEAD")
	if err != nil {
		// Shallow clones and fresh repos have no merge base; fall back to a
		// direct diff against the ref.
		mergeBase = base
	}
	out, err := run(dir, "diff", "--unified=0", "--no-color", "--no-ext-diff", strings.TrimSpace(mergeBase))
	if err != nil {
		return nil, fmt.Errorf("git diff: %w", err)
	}
	root, err := run(dir, "rev-parse", "--show-toplevel")
	if err != nil {
		return nil, fmt.Errorf("git rev-parse: %w", err)
	}
	return parse(fsutil.Canon(strings.TrimSpace(root)), out)
}

func parse(root, diff string) ([]Hunk, error) {
	var hunks []Hunk
	var file string
	sc := bufio.NewScanner(strings.NewReader(diff))
	sc.Buffer(make([]byte, 0, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "+++ "):
			p := strings.TrimPrefix(line, "+++ ")
			if p == "/dev/null" {
				file = ""
				continue
			}
			file = fsutil.Canon(filepath.Join(root, strings.TrimPrefix(p, "b/")))
		case strings.HasPrefix(line, "@@ "):
			if file == "" {
				continue
			}
			start, count, ok := parseNewRange(line)
			if !ok {
				continue
			}
			h := Hunk{File: file, StartLine: start, EndLine: start + count - 1}
			if count == 0 {
				// Pure deletion. git reports the line *before* which content was
				// removed; anchor on the following line.
				h.StartLine = start + 1
				h.EndLine = h.StartLine
				h.Deletion = true
			}
			hunks = append(hunks, h)
		}
	}
	return hunks, sc.Err()
}

// parseNewRange pulls "+c,d" out of "@@ -a,b +c,d @@ context".
func parseNewRange(line string) (start, count int, ok bool) {
	i := strings.Index(line, "+")
	if i < 0 {
		return 0, 0, false
	}
	rest := line[i+1:]
	if j := strings.IndexAny(rest, " \t"); j >= 0 {
		rest = rest[:j]
	}
	count = 1
	if c := strings.Index(rest, ","); c >= 0 {
		n, err := strconv.Atoi(rest[c+1:])
		if err != nil {
			return 0, 0, false
		}
		count = n
		rest = rest[:c]
	}
	start, err := strconv.Atoi(rest)
	if err != nil {
		return 0, 0, false
	}
	return start, count, true
}

func run(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("%s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
