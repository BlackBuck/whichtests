// Command whichtests prints the tests a diff can actually affect.
//
//	whichtests -base main
//	whichtests -base main -format go-test | sh
//	whichtests -base main -format json > selection.json
//	whichtests doctor
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/BlackBuck/whichtests/internal/gitdiff"
	"github.com/BlackBuck/whichtests/internal/graph"
	"github.com/BlackBuck/whichtests/internal/selection"
)

func main() {
	if len(os.Args) > 1 && os.Args[1] == "doctor" {
		if err := doctor(os.Args[2:]); err != nil {
			fmt.Fprintln(os.Stderr, "whichtests doctor:", err)
			os.Exit(1)
		}
		return
	}
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
		includeND    = flag.Bool("include-non-behavioral", false, "escalate on docs and assets too, instead of ignoring them")
		cache        = flag.Bool("cache", true, "reuse an on-disk graph when the module's sources are unchanged")
		tags         = flag.String("tags", "", "comma-separated build tags; must match the tags the tests will run with")
	)
	flag.Parse()

	started := time.Now()
	g, err := graph.Build(graph.Config{
		Dir: *dir, Patterns: flag.Args(), Cache: *cache, Tags: splitTags(*tags),
	})
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
	mods, err := gitdiff.Modules(*dir, *base)
	if err != nil {
		return err
	}

	res := selection.Select(g, hunks, selection.Options{
		Conservative:             *conservative,
		ConservativeOnUnresolved: *safe,
		IncludeNonBehavioral:     *includeND,
		Modules:                  mods,
	})

	if g.CacheHit {
		fmt.Fprintln(os.Stderr, "whichtests: reusing cached graph")
	}

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
	if len(r.ChangedModules) > 0 {
		fmt.Printf("%d dependency module(s) changed: %s\n",
			len(r.ChangedModules), strings.Join(truncList(r.ChangedModules, 4), ", "))
	}
	if len(r.ResolvedDecls) > 0 {
		fmt.Printf("%d declaration(s) resolved to referencing functions instead of dirtying a package\n",
			len(r.ResolvedDecls))
	}
	if r.Conservative {
		fmt.Println("selection was conservative: every test was included")
	}
	if len(r.IgnoredFiles) > 0 {
		fmt.Printf("%d changed file(s) ignored as non-behavioral (docs, assets)\n", len(r.IgnoredFiles))
	}
	if len(r.UnresolvedFiles) > 0 {
		fmt.Printf("%d changed file(s) outside the package graph:\n", len(r.UnresolvedFiles))
		for _, f := range r.UnresolvedFiles {
			fmt.Printf("  %s\n", f)
		}
	}
	return nil
}

// splitTags parses a comma-separated -tags value.
func splitTags(s string) []string {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// truncList keeps a summary line short without hiding the count.
func truncList(xs []string, n int) []string {
	if len(xs) <= n {
		return xs
	}
	return append(append([]string{}, xs[:n]...), fmt.Sprintf("and %d more", len(xs)-n))
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
