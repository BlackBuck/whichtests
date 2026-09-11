// Command whichtests prints the tests a diff can actually affect.
//
//	whichtests -base main
//	whichtests -base main -format go-test | sh
//	whichtests -base main -format json > selection.json
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"time"

	"github.com/BlackBuck/whichtests/internal/gitdiff"
	"github.com/BlackBuck/whichtests/internal/graph"
	"github.com/BlackBuck/whichtests/internal/selection"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "whichtests:", err)
		os.Exit(1)
	}
}

func run() error {
	var (
		base         = flag.String("base", "main", "git ref to diff against (merge base with HEAD)")
		dir          = flag.String("C", ".", "module directory to analyze")
		format       = flag.String("format", "text", "output format: text, json, go-test")
		conservative = flag.Bool("conservative", false, "select every test regardless of the diff")
		safe         = flag.Bool("safe", true, "fall back to every test when the diff touches files outside the package graph")
		explain      = flag.Bool("explain", false, "show why each test was selected")
	)
	flag.Parse()

	started := time.Now()
	g, err := graph.Build(graph.Config{Dir: *dir, Patterns: flag.Args()})
	if err != nil {
		return err
	}
	if g.LoadErrors > 0 {
		fmt.Fprintf(os.Stderr, "whichtests: %d package error(s); reachability may be incomplete\n", g.LoadErrors)
	}

	hunks, err := gitdiff.Changed(*dir, *base)
	if err != nil {
		return err
	}

	res := selection.Select(g, hunks, selection.Options{
		Conservative:             *conservative,
		ConservativeOnUnresolved: *safe,
	})

	switch *format {
	case "json":
		return emitJSON(res)
	case "go-test":
		return emitGoTest(res)
	case "text":
		return emitText(res, *explain, time.Since(started))
	default:
		return fmt.Errorf("unknown -format %q", *format)
	}
}

func emitText(r *selection.Result, explain bool, took time.Duration) error {
	plans := r.Plans()
	if len(r.Selected) == 0 {
		fmt.Printf("0 of %d tests selected — nothing in this diff is reachable from any test.\n", r.Total)
		return nil
	}
	for _, p := range plans {
		fmt.Printf("%s\n", p.PkgPath)
		for _, t := range p.Tests {
			fmt.Printf("  %s\n", t)
		}
	}
	if explain {
		fmt.Println()
		for _, s := range r.Selected {
			fmt.Printf("  %s.%s <- %s: %s\n", s.PkgPath, s.Name, s.Reason.Kind, s.Reason.Detail)
		}
	}
	fmt.Println()
	pct := 100 * float64(len(r.Selected)) / float64(max(r.Total, 1))
	fmt.Printf("%d of %d tests selected (%.1f%%) in %s\n", len(r.Selected), r.Total, pct, took.Round(time.Millisecond))
	fmt.Printf("%d symbol(s) changed, %d package(s) dirty at package level\n",
		len(r.ChangedSymbols), len(r.DirtyPackages))
	if r.Conservative {
		fmt.Println("selection was conservative: every test was included")
	}
	if len(r.UnresolvedFiles) > 0 {
		fmt.Printf("%d changed file(s) outside the package graph:\n", len(r.UnresolvedFiles))
		for _, f := range r.UnresolvedFiles {
			fmt.Printf("  %s\n", f)
		}
	}
	return nil
}

func emitGoTest(r *selection.Result) error {
	for _, p := range r.Plans() {
		fmt.Printf("go test -run '%s' %s\n", p.Run, p.PkgPath)
	}
	return nil
}

func emitJSON(r *selection.Result) error {
	type plan struct {
		Package string   `json:"package"`
		Run     string   `json:"run"`
		Tests   []string `json:"tests"`
	}
	out := struct {
		Selected        int      `json:"selected"`
		Total           int      `json:"total"`
		Conservative    bool     `json:"conservative"`
		Plans           []plan   `json:"plans"`
		ChangedSymbols  []string `json:"changed_symbols"`
		DirtyPackages   []string `json:"dirty_packages"`
		UnresolvedFiles []string `json:"unresolved_files"`
	}{
		Selected:        len(r.Selected),
		Total:           r.Total,
		Conservative:    r.Conservative,
		ChangedSymbols:  r.ChangedSymbols,
		DirtyPackages:   r.DirtyPackages,
		UnresolvedFiles: r.UnresolvedFiles,
	}
	for _, p := range r.Plans() {
		out.Plans = append(out.Plans, plan{Package: p.PkgPath, Run: p.Run, Tests: p.Tests})
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(out)
}
