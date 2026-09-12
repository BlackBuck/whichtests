// Package mutate measures recall by manufacturing failures.
//
// Replaying a repository's history cannot measure recall: every commit on a
// protected trunk passed CI by construction, so there are no failing tests to
// miss. Running the suite over green history burns hours and proves nothing.
//
// Fault injection produces the failures instead. For each sampled function:
//
//  1. Splice `panic(...)` in at the top of its body, so any test that actually
//     executes it fails.
//  2. Ask whichtests which tests it would select had that function changed.
//  3. Run the whole suite and collect the failures.
//  4. Any failing test outside the selection is a recall violation — a real bug
//     the tool would have let ship.
//
// One full suite run per mutant is the cost. It buys the only number that
// decides whether the tool is safe to adopt.
package mutate

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"os/exec"
	"sort"
	"strings"
	"time"

	"github.com/BlackBuck/whichtests/internal/gitdiff"
	"github.com/BlackBuck/whichtests/internal/graph"
	"github.com/BlackBuck/whichtests/internal/selection"
)

// Candidate is a function that can carry an injected fault.
type Candidate struct {
	Key        string
	PkgPath    string
	File       string
	Line       int
	BodyOffset int
	// Reaching counts the tests that reach this function, i.e. how many tests
	// whichtests would select. A candidate no test reaches is useless: the
	// mutant cannot fail anything, so it cannot expose a missed test.
	Reaching int
}

// Result is the outcome for one injected fault.
type Result struct {
	Key      string   `json:"key"`
	File     string   `json:"file"`
	Line     int      `json:"line"`
	Selected int      `json:"selected"`
	Total    int      `json:"total"`
	Failing  []string `json:"failing,omitempty"`
	Missed   []string `json:"missed,omitempty"`
	Caught   bool     `json:"caught"`
	Skipped  string   `json:"skipped,omitempty"`
	Duration string   `json:"duration"`
}

// Summary aggregates a mutation run.
type Summary struct {
	Results []Result `json:"results"`

	Injected int `json:"injected"`
	Skipped  int `json:"skipped"`
	// Killed counts mutants that at least one test caught. A mutant nothing
	// catches says nothing about selection, only about coverage.
	Killed           int `json:"killed"`
	Caught           int `json:"caught_by_selection"`
	RecallViolations int `json:"recall_violations"`
}

// Options configures a run.
type Options struct {
	Dir         string
	Patterns    []string
	Count       int
	Seed        int64
	TestTimeout time.Duration
	Progress    func(string)
}

// Run injects faults one at a time, restoring the source after each.
func Run(opts Options) (*Summary, error) {
	if opts.Count <= 0 {
		opts.Count = 10
	}
	if opts.TestTimeout == 0 {
		opts.TestTimeout = 25 * time.Minute
	}

	g, err := graph.Build(graph.Config{Dir: opts.Dir, Patterns: opts.Patterns})
	if err != nil {
		return nil, err
	}

	cands := Candidates(g)
	if len(cands) == 0 {
		return nil, fmt.Errorf("no mutable functions reachable from any test")
	}
	rng := rand.New(rand.NewSource(opts.Seed))
	rng.Shuffle(len(cands), func(i, j int) { cands[i], cands[j] = cands[j], cands[i] })
	if len(cands) > opts.Count {
		cands = cands[:opts.Count]
	}

	sum := &Summary{}
	for i, c := range cands {
		// Counting reachers is a backward walk, so it is only worth doing for
		// the handful of candidates actually sampled.
		c.Reaching = len(g.Reaching(map[string]bool{c.Key: true}))
		if opts.Progress != nil {
			opts.Progress(fmt.Sprintf("[%d/%d] %s (%d tests reach it)", i+1, len(cands), c.Key, c.Reaching))
		}
		r := inject(g, c, opts)
		sum.Results = append(sum.Results, r)
		if r.Skipped != "" {
			sum.Skipped++
			continue
		}
		sum.Injected++
		if len(r.Failing) > 0 {
			sum.Killed++
		}
		if r.Caught {
			sum.Caught++
		}
		if len(r.Missed) > 0 {
			sum.RecallViolations++
		}
	}
	return sum, nil
}

// Candidates returns every function a fault can be injected into: it has a
// body, it is not itself a test, and at least one test reaches it.
func Candidates(g *graph.Graph) []Candidate {
	testKeys := make(map[string]bool, len(g.Tests))
	for _, t := range g.Tests {
		testKeys[t.PkgPath+"."+t.Name] = true
	}

	var out []Candidate
	for file, spans := range g.Spans {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		for _, s := range spans {
			// A symbol absent from the call graph is dead code as far as the
			// suite is concerned: the mutant could not fail anything, so it
			// could not expose a missed test either.
			if s.BodyOffset == 0 || testKeys[s.Key] || !g.InCallGraph(s.Key) {
				continue
			}
			// Skip the generated test main package and synthesised inits.
			// Editing _testmain.go mutates a file the toolchain regenerates,
			// so the fault never reaches a build.
			if synthetic(s.PkgPath, s.Key) {
				continue
			}
			out = append(out, Candidate{
				Key:        s.Key,
				PkgPath:    s.PkgPath,
				File:       file,
				Line:       s.StartLine,
				BodyOffset: s.BodyOffset,
			})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Key < out[j].Key })
	return out
}

// synthetic reports whether a symbol belongs to generated scaffolding rather
// than to source a person wrote.
func synthetic(pkgPath, key string) bool {
	return strings.HasSuffix(pkgPath, ".test") ||
		strings.Contains(key, "#") ||
		strings.HasSuffix(key, ".init")
}

const marker = `panic("whichtests injected fault")` + "\n"

func inject(g *graph.Graph, c Candidate, opts Options) Result {
	started := time.Now()
	r := Result{Key: c.Key, File: c.File, Line: c.Line, Total: len(g.Tests)}

	orig, err := os.ReadFile(c.File)
	if err != nil {
		r.Skipped = "read: " + err.Error()
		return r
	}
	if c.BodyOffset > len(orig) {
		r.Skipped = "stale body offset"
		return r
	}
	mutated := make([]byte, 0, len(orig)+len(marker))
	mutated = append(mutated, orig[:c.BodyOffset]...)
	mutated = append(mutated, marker...)
	mutated = append(mutated, orig[c.BodyOffset:]...)

	info, err := os.Stat(c.File)
	if err != nil {
		r.Skipped = "stat: " + err.Error()
		return r
	}
	if err := os.WriteFile(c.File, mutated, info.Mode()); err != nil {
		r.Skipped = "write: " + err.Error()
		return r
	}
	// The source must come back even if the test run panics or is interrupted.
	defer os.WriteFile(c.File, orig, info.Mode())

	// Run the real selector over a synthetic hunk on this function, rather
	// than querying reachability directly. Those are not the same thing: the
	// selector adds the fallbacks -- reflection boundaries in particular -- and
	// a harness that measures the internal instead of the product reports
	// violations the tool does not actually have.
	hunks := []gitdiff.Hunk{{File: c.File, StartLine: c.Line, EndLine: c.Line}}
	sel := selection.Select(g, hunks, selection.Options{})
	selected := make(map[string]bool, len(sel.Selected))
	for _, s := range sel.Selected {
		selected[s.PkgPath+"."+s.Name] = true
	}
	r.Selected = len(selected)

	failing, err := runSuite(opts.Dir, opts.TestTimeout)
	if err != nil {
		r.Skipped = "suite: " + err.Error()
		return r
	}
	for f := range failing {
		r.Failing = append(r.Failing, f)
		if !selected[f] {
			r.Missed = append(r.Missed, f)
		}
	}
	sort.Strings(r.Failing)
	sort.Strings(r.Missed)
	r.Caught = len(r.Failing) > 0 && len(r.Missed) == 0
	r.Duration = time.Since(started).Round(time.Second).String()
	return r
}

func runSuite(dir string, timeout time.Duration) (map[string]bool, error) {
	// No -count=1, deliberately. Go keys the test cache on the compiled test
	// binary, so a package the mutant does not affect has identical inputs and
	// its cached pass is correct; a package it does affect recompiles and
	// re-runs. Failures are never cached. That makes each mutant cost the
	// affected packages rather than all 1714 tests, which is the difference
	// between a hundred mutants and a dozen.
	cmd := exec.Command("go", "test", "-json", "-timeout", timeout.String(), "./...")
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	_ = cmd.Run() // a failing suite is the point

	failing := make(map[string]bool)
	sc := bufio.NewScanner(&stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var saw bool
	for sc.Scan() {
		var ev struct {
			Action, Package, Test string
		}
		if err := json.Unmarshal(sc.Bytes(), &ev); err != nil {
			continue
		}
		saw = true
		if ev.Action != "fail" || ev.Test == "" {
			continue
		}
		name := ev.Test
		if i := strings.Index(name, "/"); i >= 0 {
			name = name[:i]
		}
		failing[ev.Package+"."+name] = true
	}
	if !saw {
		return nil, fmt.Errorf("no test events")
	}
	return failing, nil
}
