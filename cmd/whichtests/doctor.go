package main

import (
	"flag"
	"fmt"
	"sort"
	"strings"

	"github.com/BlackBuck/whichtests/internal/graph"
)

// doctor answers "is this tool worth anything on my repository" before anyone
// spends time wiring it in.
//
// The answer turns almost entirely on one number. `go test` already skips
// packages a change cannot affect, so the only headroom whichtests has on a
// warm cache is selecting *within* a changed package -- and a repository whose
// packages hold four tests each has nothing to select from. Reporting that
// honestly is more useful than a tool that quietly saves nobody anything.
func doctor(args []string) error {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	dir := fs.String("C", ".", "module directory to analyze")
	cache := fs.Bool("cache", true, "reuse an on-disk graph when the module's sources are unchanged")
	fat := fs.Int("fat", 20, "tests per package above which a package has real headroom")
	tags := fs.String("tags", "", "comma-separated build tags")
	sample := fs.Int("sample", 200, "functions to sample when estimating the typical selection")
	if err := fs.Parse(args); err != nil {
		return err
	}

	g, err := graph.Build(graph.Config{
		Dir: *dir, Patterns: fs.Args(), Cache: *cache, Tags: splitTags(*tags),
	})
	if err != nil {
		return err
	}

	perPkg := make(map[string]int)
	for _, t := range g.Tests {
		perPkg[t.PkgPath]++
	}
	if len(perPkg) == 0 {
		fmt.Println("no tests found")
		return nil
	}

	counts := make([]int, 0, len(perPkg))
	total := 0
	for _, n := range perPkg {
		counts = append(counts, n)
		total += n
	}
	sort.Ints(counts)

	pct := func(p float64) int { return counts[min(int(float64(len(counts))*p), len(counts)-1)] }
	fmt.Printf("%d packages with tests, %d tests total\n", len(perPkg), total)
	fmt.Printf("tests per package: median %d, mean %.1f, p90 %d, max %d\n\n",
		counts[len(counts)/2], float64(total)/float64(len(counts)), pct(0.9), counts[len(counts)-1])

	type pkg struct {
		path string
		n    int
	}
	fattest := make([]pkg, 0, len(perPkg))
	inFat := 0
	for p, n := range perPkg {
		fattest = append(fattest, pkg{p, n})
		if n >= *fat {
			inFat += n
		}
	}
	sort.Slice(fattest, func(i, j int) bool {
		if fattest[i].n != fattest[j].n {
			return fattest[i].n > fattest[j].n
		}
		return fattest[i].path < fattest[j].path
	})

	fmt.Printf("fattest packages:\n")
	for i, p := range fattest {
		if i >= 3 || p.n < *fat {
			break
		}
		fmt.Printf("  %5d  %s\n", p.n, p.path)
	}
	if fattest[0].n < *fat {
		fmt.Printf("  none with %d or more tests\n", *fat)
	}
	fmt.Println()

	// Package size looked like the thing that mattered and is not. gin holds
	// 96% of its tests in fat packages, and a change to almost any function
	// there selects 454 of 456 tests, because every test builds an Engine and
	// reaches nearly everything. cobra is the same story. What actually
	// predicts the saving is how much the tests *differ* in what they reach,
	// so measure that directly rather than guessing from a proxy.
	keys := sampleSymbols(g, *sample)
	if len(keys) == 0 {
		fmt.Println("no analysable functions found")
		return nil
	}
	ratios := make([]float64, 0, len(keys))
	for _, k := range keys {
		ratios = append(ratios, float64(len(g.Reaching(map[string]bool{k: true})))/float64(total))
	}
	sort.Float64s(ratios)
	med := ratios[len(ratios)/2]
	p10 := ratios[len(ratios)/10]
	p90pick := ratios[min(int(float64(len(ratios))*0.9), len(ratios)-1)]

	fmt.Printf("sampled %d functions; a change to one selects:\n", len(ratios))
	fmt.Printf("  p10 %.1f%%   median %.1f%%   p90 %.1f%%   of the suite\n\n",
		100*p10, 100*med, 100*p90pick)

	switch {
	case med <= 0.10:
		fmt.Println("Verdict: worth trying. A typical change reaches a small slice of")
		fmt.Println("the suite, which is exactly what this can skip.")
	case med <= 0.40:
		fmt.Println("Verdict: marginal. A typical change reaches a sizeable fraction of")
		fmt.Println("the suite, so the saving depends on where your changes land.")
	default:
		fmt.Println("Verdict: little to gain. Your tests mostly reach the same code, so")
		fmt.Println("there is little for reachability to tell apart. That is common in a")
		fmt.Println("cohesive package where every test builds the same object.")
	}
	fmt.Println()

	fmt.Println()
	fmt.Println("Cold CI is a separate question. With nothing cached `go test` runs")
	fmt.Printf("all %d tests no matter how small the diff, so the comparison there is\n", total)
	fmt.Println("against the whole suite rather than against the cache. Persisting")
	fmt.Println("GOCACHE between runs is the cheaper thing to try first.")
	return nil
}

// sampleSymbols picks real source functions, spread across packages so one
// enormous package cannot dominate the estimate.
func sampleSymbols(g *graph.Graph, n int) []string {
	byPkg := map[string][]string{}
	seen := map[string]bool{}
	for file, spans := range g.Spans {
		if strings.HasSuffix(file, "_test.go") {
			continue
		}
		for _, sp := range spans {
			if seen[sp.Key] || sp.BodyOffset == 0 || !g.InCallGraph(sp.Key) {
				continue
			}
			if strings.Contains(sp.Key, "#") || strings.HasSuffix(sp.Key, ".init") {
				continue
			}
			seen[sp.Key] = true
			byPkg[sp.PkgPath] = append(byPkg[sp.PkgPath], sp.Key)
		}
	}
	pkgs := make([]string, 0, len(byPkg))
	for p := range byPkg {
		sort.Strings(byPkg[p])
		pkgs = append(pkgs, p)
	}
	sort.Strings(pkgs)

	var out []string
	for round := 0; len(out) < n; round++ {
		progressed := false
		for _, p := range pkgs {
			if round < len(byPkg[p]) {
				out = append(out, byPkg[p][round])
				progressed = true
				if len(out) >= n {
					return out
				}
			}
		}
		if !progressed {
			break
		}
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
