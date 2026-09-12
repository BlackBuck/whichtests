// Command whichtests-mutate measures recall by injecting faults.
//
// Replaying green history cannot measure recall — every merged commit passed.
// This manufactures the failures instead: it panics one function at a time,
// runs the whole suite, and checks that every test which failed was one
// whichtests would have selected.
//
//	whichtests-mutate -C ../cli -n 15
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/BlackBuck/whichtests/internal/mutate"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "whichtests-mutate:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		dir     = flag.String("C", ".", "module directory to analyze")
		n       = flag.Int("n", 10, "number of faults to inject")
		seed    = flag.Int64("seed", 1, "sampling seed, for reproducible runs")
		timeout = flag.Duration("test-timeout", 25*time.Minute, "timeout for one `go test ./...`")
		jsonOut = flag.String("json", "", "write the full per-mutant result to this file")
		quiet   = flag.Bool("quiet", false, "suppress per-mutant progress")
	)
	flag.Parse()

	opts := mutate.Options{
		Dir:         *dir,
		Patterns:    flag.Args(),
		Count:       *n,
		Seed:        *seed,
		TestTimeout: *timeout,
	}
	if !*quiet {
		opts.Progress = func(msg string) { fmt.Fprintln(os.Stderr, msg) }
	}

	sum, err := mutate.Run(opts)
	if err != nil {
		return err
	}
	report(sum)

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

	if sum.RecallViolations > 0 {
		return fmt.Errorf("%d mutant(s) were caught by a test whichtests would not have run", sum.RecallViolations)
	}
	return nil
}

func report(s *mutate.Summary) {
	fmt.Println()
	fmt.Printf("injected %d fault(s), skipped %d\n", s.Injected, s.Skipped)
	if s.Injected == 0 {
		return
	}
	fmt.Printf("killed by the suite: %d (a mutant nothing catches says nothing about selection)\n", s.Killed)
	if s.Killed == 0 {
		fmt.Println("no mutant was caught by any test, so recall is unmeasured.")
		return
	}
	fmt.Printf("recall: %d of %d killed mutants were caught inside the selection\n", s.Caught, s.Killed)

	if s.RecallViolations == 0 {
		fmt.Println("no recall violations: every failing test was one whichtests would have run")
	} else {
		fmt.Printf("\nRECALL VIOLATIONS in %d mutant(s):\n", s.RecallViolations)
		for _, r := range s.Results {
			if len(r.Missed) == 0 {
				continue
			}
			fmt.Printf("  %s (%s:%d)\n", r.Key, r.File, r.Line)
			fmt.Printf("      selected %d test(s), but these failing tests were not among them:\n", r.Selected)
			for _, m := range r.Missed {
				fmt.Printf("        %s\n", m)
			}
		}
	}

	fmt.Println()
	fmt.Println("per mutant:")
	for _, r := range s.Results {
		if r.Skipped != "" {
			fmt.Printf("  SKIP  %-55s %s\n", trunc(r.Key, 55), r.Skipped)
			continue
		}
		status := "killed"
		if len(r.Failing) == 0 {
			status = "survived"
		}
		if len(r.Missed) > 0 {
			status = "MISSED"
		}
		fmt.Printf("  %-9s %-55s selected %4d/%-4d failing %3d  %s\n",
			status, trunc(r.Key, 55), r.Selected, r.Total, len(r.Failing), r.Duration)
	}
}

func trunc(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return "..." + s[len(s)-n+3:]
}
