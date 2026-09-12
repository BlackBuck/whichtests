// Package gitdiff turns a git revision range into line ranges on the new side
// of the diff.
package gitdiff

import (
	"bufio"
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"sort"
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

// ModuleChange describes what a diff did to go.mod and go.sum.
type ModuleChange struct {
	// Modules are the module paths whose requirement lines changed.
	Modules []string
	// Wildcard means the change cannot be attributed to particular modules --
	// a `go` or `toolchain` directive bump, or a line we could not parse.
	// Everything has to run.
	Wildcard bool
	// Touched is true if go.mod or go.sum changed at all.
	Touched bool
}

// Modules reports which dependencies a diff altered.
//
// A dependency bump can change anything, so the honest default is to run the
// whole suite. But "anything" is bounded by what actually imports the bumped
// module: a tunnelling library used by one command cannot break the tests for
// an unrelated one. This reads the requirement lines rather than the file's
// line ranges, because a version bump's diff hunk says nothing about which
// module the line belongs to.
func Modules(dir, base string) (ModuleChange, error) {
	var mc ModuleChange
	mergeBase, err := run(dir, "merge-base", base, "HEAD")
	if err != nil {
		mergeBase = base
	}
	out, err := run(dir, "diff", "--unified=0", "--no-color", "--no-ext-diff",
		strings.TrimSpace(mergeBase), "--", "go.mod", "go.sum")
	if err != nil {
		return mc, fmt.Errorf("git diff go.mod: %w", err)
	}
	if strings.TrimSpace(out) == "" {
		return mc, nil
	}
	mc.Touched = true

	seen := make(map[string]bool)
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Text()
		if len(line) == 0 || (line[0] != '+' && line[0] != '-') {
			continue
		}
		if strings.HasPrefix(line, "+++") || strings.HasPrefix(line, "---") {
			continue
		}
		mod, ok := moduleOfLine(line[1:])
		if !ok {
			continue
		}
		if mod == "" {
			// A `go` or `toolchain` directive: the language or toolchain
			// version changed, which can affect every package.
			mc.Wildcard = true
			continue
		}
		if !seen[mod] {
			seen[mod] = true
			mc.Modules = append(mc.Modules, mod)
		}
	}
	sort.Strings(mc.Modules)
	if len(mc.Modules) == 0 {
		mc.Wildcard = true
	}
	return mc, sc.Err()
}

// moduleOfLine pulls a module path out of a go.mod or go.sum line. It returns
// ok=false for lines that carry no dependency information (blank lines, braces,
// comments) and mod=="" for directives that invalidate everything.
func moduleOfLine(s string) (mod string, ok bool) {
	s = strings.TrimSpace(s)
	if i := strings.Index(s, "//"); i >= 0 {
		s = strings.TrimSpace(s[:i])
	}
	if s == "" || s == ")" || s == "(" {
		return "", false
	}
	f := strings.Fields(s)
	if len(f) == 0 {
		return "", false
	}
	switch f[0] {
	case "go", "toolchain":
		return "", true // language or toolchain bump: nothing is safe to skip
	case "module":
		return "", false
	case "require", "exclude", "replace", "retract":
		f = f[1:]
		if len(f) == 0 {
			return "", false
		}
	}
	// go.mod: "path v1.2.3"; go.sum: "path v1.2.3 h1:..." or "path v1.2.3/go.mod h1:..."
	if len(f) >= 2 && strings.HasPrefix(f[1], "v") && strings.ContainsAny(f[0], "./") {
		return f[0], true
	}
	return "", false
}
