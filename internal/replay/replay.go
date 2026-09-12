// Package replay measures whichtests against real history.
//
// Two numbers matter, and they are not equally cheap:
//
//   - Selection ratio: what fraction of the suite a commit selects. Static
//     only, no test execution, runs over hundreds of commits in minutes.
//   - Recall: whether the selected subset still contains every test the full
//     suite catches failing. This is the number that decides whether the tool
//     is safe to adopt, and it costs one or two full suite runs per commit.
//     Opt in with Options.Verify and expect it to take hours.
//
// A selection ratio without a recall number is marketing, so the harness
// reports them together and never implies the first says anything about the
// second.
package replay

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/BlackBuck/whichtests/internal/fsutil"
	"github.com/BlackBuck/whichtests/internal/gitdiff"
	"github.com/BlackBuck/whichtests/internal/graph"
	"github.com/BlackBuck/whichtests/internal/selection"
)

// Options configures a replay run.
type Options struct {
	Repo    string   // path to the git repository to replay
	Module  string   // module directory relative to Repo (default ".")
	Rev     string   // revision to walk back from (default "HEAD")
	Commits int      // how many commits to replay (default 50)
	Pattern []string // package patterns for the analysis

	// Verify runs the test suite at each commit and checks that every failing
	// test was selected. Expensive.
	Verify bool
	// Baseline additionally runs the suite at the parent commit so only *newly*
	// failing tests count. Without it a test that was already broken before the
	// diff is wrongly scored as a recall violation. Doubles the cost.
	Baseline bool
	// IncludeNonBehavioral disables the documentation/asset filter.
	IncludeNonBehavioral bool
	// TestTimeout bounds a single `go test ./...` invocation.
	TestTimeout time.Duration

	// WorkDir holds the scratch worktree. Created if empty, removed on cleanup.
	WorkDir string

	// Progress receives one line per commit as the replay advances.
	Progress func(string)
}

// CommitResult is the outcome for a single replayed commit.
type CommitResult struct {
	SHA     string `json:"sha"`
	Parent  string `json:"parent"`
	Subject string `json:"subject"`

	Selected       int           `json:"selected"`
	Total          int           `json:"total"`
	Ratio          float64       `json:"ratio"`
	Conservative   bool          `json:"conservative"`
	ChangedSymbols int           `json:"changed_symbols"`
	DirtyPackages  int           `json:"dirty_packages"`
	ResolvedDecls  int           `json:"resolved_decls"`
	ChangedModules int           `json:"changed_modules"`
	AnalysisTime   time.Duration `json:"analysis_ns"`

	// Unresolved records the files that pushed this commit onto the
	// conservative path. Without them a replay reports "80% of commits ran
	// everything" and gives you no way to find out why.
	Unresolved []string `json:"unresolved,omitempty"`

	// Verify-mode fields.
	Verified bool     `json:"verified"`
	Failing  []string `json:"failing,omitempty"`
	Missed   []string `json:"missed,omitempty"`

	Skipped string `json:"skipped,omitempty"`
}

// Summary aggregates a replay.
type Summary struct {
	Results []CommitResult `json:"results"`

	Replayed     int     `json:"replayed"`
	Skipped      int     `json:"skipped"`
	MeanRatio    float64 `json:"mean_ratio"`
	MedianRatio  float64 `json:"median_ratio"`
	Conservative int     `json:"conservative_commits"`

	DeclResolved      int `json:"commits_using_decl_resolution"`
	ModNarrowed       int `json:"commits_using_module_narrowing"`
	Verified          int `json:"verified_commits"`
	CommitsWithFailed int `json:"commits_with_failures"`
	RecallViolations  int `json:"recall_violations"`
}

// Run replays history and returns the aggregate.
func Run(opts Options) (*Summary, error) {
	if opts.Rev == "" {
		opts.Rev = "HEAD"
	}
	if opts.Commits <= 0 {
		opts.Commits = 50
	}
	if opts.Module == "" {
		opts.Module = "."
	}
	if opts.TestTimeout == 0 {
		opts.TestTimeout = 20 * time.Minute
	}

	commits, err := listCommits(opts.Repo, opts.Rev, opts.Commits)
	if err != nil {
		return nil, err
	}

	work := opts.WorkDir
	if work == "" {
		work, err = os.MkdirTemp("", "whichtests-replay-")
		if err != nil {
			return nil, err
		}
	}
	// A detached worktree keeps the caller's checkout untouched; replaying
	// history by checking out commits in the user's own repo would trash
	// uncommitted work.
	wt := filepath.Join(work, "wt")
	if out, err := git(opts.Repo, "worktree", "add", "--detach", wt, commits[0].SHA); err != nil {
		return nil, fmt.Errorf("create worktree: %w (%s)", err, out)
	}
	defer func() {
		_, _ = git(opts.Repo, "worktree", "remove", "--force", wt)
		if opts.WorkDir == "" {
			_ = os.RemoveAll(work)
		}
	}()

	sum := &Summary{}
	moduleDir := filepath.Join(wt, opts.Module)
	// Selection reports canonical paths, so the worktree root has to be
	// canonical too or every relative path comes out as ../../../..
	canonWT := fsutil.Canon(wt)

	for i, c := range commits {
		progress(opts, fmt.Sprintf("[%d/%d] %s %s", i+1, len(commits), short(c.SHA), c.Subject))

		r := CommitResult{SHA: c.SHA, Parent: c.Parent, Subject: c.Subject}
		if c.Parent == "" {
			sum.skip(&r, "no parent commit")
			continue
		}
		if _, err := git(wt, "checkout", "--detach", "--force", c.SHA); err != nil {
			sum.skip(&r, "checkout failed: "+err.Error())
			continue
		}

		started := time.Now()
		g, err := graph.Build(graph.Config{Dir: moduleDir, Patterns: opts.Pattern})
		if err != nil {
			sum.skip(&r, "analysis failed: "+err.Error())
			continue
		}
		hunks, err := gitdiff.Changed(moduleDir, c.Parent)
		if err != nil {
			sum.skip(&r, "diff failed: "+err.Error())
			continue
		}
		mods, err := gitdiff.Modules(moduleDir, c.Parent)
		if err != nil {
			sum.skip(&r, "go.mod diff failed: "+err.Error())
			continue
		}
		// ConservativeOnUnresolved mirrors the CLI default, so the measured
		// ratio is the one a real user would actually get.
		res := selection.Select(g, hunks, selection.Options{
			ConservativeOnUnresolved: true,
			IncludeNonBehavioral:     opts.IncludeNonBehavioral,
			Modules:                  mods,
		})
		r.AnalysisTime = time.Since(started)
		r.Selected = len(res.Selected)
		r.Total = res.Total
		r.Conservative = res.Conservative
		r.ChangedSymbols = len(res.ChangedSymbols)
		r.DirtyPackages = len(res.DirtyPackages)
		r.ResolvedDecls = len(res.ResolvedDecls)
		r.ChangedModules = len(res.ChangedModules)
		for _, f := range res.UnresolvedFiles {
			if rel, err := filepath.Rel(canonWT, f); err == nil {
				r.Unresolved = append(r.Unresolved, rel)
			} else {
				r.Unresolved = append(r.Unresolved, f)
			}
		}
		if r.Total > 0 {
			r.Ratio = float64(r.Selected) / float64(r.Total)
		}

		if opts.Verify {
			failing, err := verify(opts, wt, moduleDir, c)
			if err != nil {
				r.Skipped = "verify failed: " + err.Error()
			} else {
				r.Verified = true
				r.Failing = failing
				r.Missed = missed(failing, res)
			}
		}

		sum.Results = append(sum.Results, r)
		sum.Replayed++
	}

	aggregate(sum)
	return sum, nil
}

func (s *Summary) skip(r *CommitResult, reason string) {
	r.Skipped = reason
	s.Results = append(s.Results, *r)
	s.Skipped++
}

// verify returns the tests failing at c that were not already failing at its
// parent. Only newly failing tests are attributable to the diff.
func verify(opts Options, wt, moduleDir string, c commit) ([]string, error) {
	failAt, err := runSuite(moduleDir, opts.TestTimeout)
	if err != nil {
		return nil, err
	}
	if len(failAt) == 0 || !opts.Baseline {
		return sortedSet(failAt), nil
	}
	if _, err := git(wt, "checkout", "--detach", "--force", c.Parent); err != nil {
		return nil, fmt.Errorf("checkout parent: %w", err)
	}
	failBefore, err := runSuite(moduleDir, opts.TestTimeout)
	if err != nil {
		return nil, err
	}
	for k := range failBefore {
		delete(failAt, k)
	}
	return sortedSet(failAt), nil
}

// missed reports failing tests the selection would have skipped. A non-empty
// result is a recall violation: a bug that would have shipped.
func missed(failing []string, res *selection.Result) []string {
	if res.Conservative {
		return nil
	}
	sel := make(map[string]bool, len(res.Selected))
	for _, s := range res.Selected {
		sel[s.PkgPath+"."+s.Name] = true
	}
	var out []string
	for _, f := range failing {
		if !sel[f] {
			out = append(out, f)
		}
	}
	return out
}

// runSuite runs the full suite and returns the failing tests as
// "importpath.TestName". Subtests collapse into their parent, since -run
// selection is per top-level test anyway.
func runSuite(dir string, timeout time.Duration) (map[string]bool, error) {
	// No -count=1. Go keys the test cache on the compiled test binary, so a
	// package this commit does not affect has identical inputs and its cached
	// pass is correct, while failures are never cached. That turns each
	// verified commit from a full suite run into the affected packages.
	cmd := exec.Command("go", "test", "-json",
		"-timeout", timeout.String(), "./...")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	// A failing suite is the expected case here, so the exit code is ignored
	// and only unparseable output counts as an error.
	_ = cmd.Run()

	failing := make(map[string]bool)
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var sawEvent bool
	for sc.Scan() {
		var ev struct {
			Action  string `json:"Action"`
			Package string `json:"Package"`
			Test    string `json:"Test"`
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		sawEvent = true
		if ev.Action != "fail" || ev.Test == "" {
			continue
		}
		name := ev.Test
		if i := strings.Index(name, "/"); i >= 0 {
			name = name[:i]
		}
		failing[ev.Package+"."+name] = true
	}
	if !sawEvent {
		return nil, fmt.Errorf("no test events: %s", truncate(stderr.String(), 300))
	}
	return failing, nil
}

type commit struct {
	SHA     string
	Parent  string
	Subject string
}

// listCommits walks first-parent history, which is the merge-to-main sequence
// a CI system would actually have analysed.
func listCommits(repo, rev string, n int) ([]commit, error) {
	out, err := git(repo, "log", "--first-parent", "-n", fmt.Sprint(n),
		"--format=%H%x00%P%x00%s", rev)
	if err != nil {
		return nil, fmt.Errorf("git log: %w", err)
	}
	var commits []commit
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		parts := strings.Split(line, "\x00")
		if len(parts) != 3 {
			continue
		}
		c := commit{SHA: parts[0], Subject: parts[2]}
		if parents := strings.Fields(parts[1]); len(parents) > 0 {
			c.Parent = parents[0]
		}
		commits = append(commits, c)
	}
	if len(commits) == 0 {
		return nil, fmt.Errorf("no commits found for %s", rev)
	}
	return commits, nil
}

func aggregate(s *Summary) {
	var ratios []float64
	for _, r := range s.Results {
		if r.Skipped != "" {
			continue
		}
		ratios = append(ratios, r.Ratio)
		if r.Conservative {
			s.Conservative++
		}
		if r.ResolvedDecls > 0 {
			s.DeclResolved++
		}
		if r.ChangedModules > 0 && !r.Conservative {
			s.ModNarrowed++
		}
		if r.Verified {
			s.Verified++
			if len(r.Failing) > 0 {
				s.CommitsWithFailed++
			}
			if len(r.Missed) > 0 {
				s.RecallViolations++
			}
		}
	}
	if len(ratios) == 0 {
		return
	}
	var total float64
	for _, r := range ratios {
		total += r
	}
	s.MeanRatio = total / float64(len(ratios))
	sort.Float64s(ratios)
	s.MedianRatio = ratios[len(ratios)/2]
}

func git(dir string, args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %w: %s",
			strings.Join(args, " "), err, truncate(strings.TrimSpace(stderr.String()), 200))
	}
	return stdout.String(), nil
}

func progress(o Options, msg string) {
	if o.Progress != nil {
		o.Progress(msg)
	}
}

func sortedSet(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n] + "..."
	}
	return s
}
