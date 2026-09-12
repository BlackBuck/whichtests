// Command whichtests-replay measures whichtests against a repository's history.
//
//	# cheap: selection ratio over the last 100 merges
//	whichtests-replay -repo ../grafana -n 100
//
//	# expensive: also run the suite at each commit and check recall
//	whichtests-replay -repo ../grafana -n 20 -verify -baseline
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/BlackBuck/whichtests/internal/replay"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "whichtests-replay:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		repo      = flag.String("repo", ".", "git repository to replay")
		module    = flag.String("module", ".", "module directory relative to -repo")
		rev       = flag.String("rev", "HEAD", "revision to walk back from")
		n         = flag.Int("n", 50, "number of commits to replay")
		verify    = flag.Bool("verify", false, "run the suite at each commit to measure recall (slow)")
		baseline  = flag.Bool("baseline", false, "also run the suite at the parent so only newly failing tests count (slower)")
		timeout   = flag.Duration("test-timeout", 20*time.Minute, "timeout for one `go test ./...`")
		jsonOut   = flag.String("json", "", "write the full per-commit result to this file")
		quiet     = flag.Bool("quiet", false, "suppress per-commit progress")
		includeND = flag.Bool("include-non-behavioral", false, "escalate on docs and assets too")
	)
	flag.Parse()

	if *baseline && !*verify {
		return fmt.Errorf("-baseline requires -verify")
	}

	opts := replay.Options{
		Repo:                 *repo,
		Module:               *module,
		Rev:                  *rev,
		Commits:              *n,
		Verify:               *verify,
		Baseline:             *baseline,
		TestTimeout:          *timeout,
		IncludeNonBehavioral: *includeND,
		Pattern:              flag.Args(),
	}
	if !*quiet {
		opts.Progress = func(msg string) { fmt.Fprintln(os.Stderr, msg) }
	}

	sum, err := replay.Run(opts)
	if err != nil {
		return err
	}

	report(sum, *verify)

	if *jsonOut != "" {
		f, err := os.Create(*jsonOut)
		if err != nil {
			return err
		}
		defer f.Close()
		enc := json.NewEncoder(f)
		enc.SetIndent("", "  ")
		if err := enc.Encode(sum); err != nil {
			return err
		}
		fmt.Printf("\nfull results written to %s\n", *jsonOut)
	}

	// A recall violation means the tool would have let a real failure through.
	// That is a hard failure, not a statistic.
	if sum.RecallViolations > 0 {
		return fmt.Errorf("%d commit(s) had failing tests that would not have been selected", sum.RecallViolations)
	}
	return nil
}

type cause struct {
	ext     string
	commits int
}

// conservativeCauses groups the unresolved files by extension, which is the
// fastest way to see whether the escalations are legitimate (go.sum, testdata)
// or just documentation churn that cannot affect any test.
func conservativeCauses(s *replay.Summary) []cause {
	byExt := map[string]map[string]bool{}
	for _, r := range s.Results {
		for _, f := range r.Unresolved {
			ext := filepath.Ext(f)
			if ext == "" {
				ext = filepath.Base(f)
			}
			if byExt[ext] == nil {
				byExt[ext] = map[string]bool{}
			}
			byExt[ext][r.SHA] = true
		}
	}
	out := make([]cause, 0, len(byExt))
	for ext, shas := range byExt {
		out = append(out, cause{ext, len(shas)})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].commits != out[j].commits {
			return out[i].commits > out[j].commits
		}
		return out[i].ext < out[j].ext
	})
	if len(out) > 10 {
		out = out[:10]
	}
	return out
}

func report(s *replay.Summary, verify bool) {
	fmt.Println()
	fmt.Printf("replayed %d commit(s), skipped %d\n", s.Replayed, s.Skipped)
	if s.Replayed == 0 {
		return
	}
	fmt.Printf("selection ratio: mean %.1f%%, median %.1f%%\n", 100*s.MeanRatio, 100*s.MedianRatio)
	fmt.Printf("conservative (ran everything): %d of %d commits (%.1f%%)\n",
		s.Conservative, s.Replayed, 100*float64(s.Conservative)/float64(s.Replayed))

	if s.Conservative > 0 {
		fmt.Println()
		fmt.Println("what pushed commits onto the conservative path:")
		for _, e := range conservativeCauses(s) {
			fmt.Printf("  %-14s %2d commit(s)\n", e.ext, e.commits)
		}
	}

	fmt.Println()
	fmt.Println("worst offenders (highest selection ratio, non-conservative):")
	shown := 0
	for _, r := range s.Results {
		if r.Skipped != "" || r.Conservative || r.Ratio < 0.5 || shown >= 5 {
			continue
		}
		fmt.Printf("  %.0f%%  %s  %s\n", 100*r.Ratio, r.SHA[:8], r.Subject)
		shown++
	}
	if shown == 0 {
		fmt.Println("  none above 50%")
	}

	if !verify {
		fmt.Println()
		fmt.Println("recall was NOT measured. A selection ratio alone says nothing about")
		fmt.Println("whether the skipped tests would have caught anything; re-run with")
		fmt.Println("-verify -baseline to find out.")
		return
	}

	fmt.Println()
	fmt.Printf("recall: %d commit(s) verified, %d had failing tests\n", s.Verified, s.CommitsWithFailed)
	if s.CommitsWithFailed == 0 {
		fmt.Println("no commit in this range had a failing test, so recall is unmeasured.")
		fmt.Println("pick a range with known-bad commits, or inject faults.")
		return
	}
	if s.RecallViolations == 0 {
		fmt.Printf("recall 100%%: every failing test was selected in all %d commit(s)\n", s.CommitsWithFailed)
		return
	}
	fmt.Printf("RECALL VIOLATIONS in %d commit(s):\n", s.RecallViolations)
	for _, r := range s.Results {
		if len(r.Missed) == 0 {
			continue
		}
		fmt.Printf("  %s %s\n", r.SHA[:8], r.Subject)
		for _, m := range r.Missed {
			fmt.Printf("      missed: %s\n", m)
		}
	}
}
