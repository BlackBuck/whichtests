package main

import (
	"flag"
	"fmt"
	"math/rand"
	"sort"
	"strings"

	"github.com/BlackBuck/whichtests/internal/gitdiff"
	"github.com/BlackBuck/whichtests/internal/graph"
	"github.com/BlackBuck/whichtests/internal/selection"
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
	seed := fs.Int64("seed", 1, "sampling seed, for reproducible runs")
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
	keys := sampleSymbols(g, *sample, *seed)
	if len(keys) == 0 {
		fmt.Println("no analysable functions found")
		return nil
	}
	// The honest comparison is not against the whole suite but against what
	// `go test` already does: it re-runs the changed package and every package
	// whose test binary imports it, and skips the rest from cache. Both sides
	// are computable without running a single test.
	baselineFor := func(pkg string) int {
		n := 0
		for _, t := range g.Tests {
			if t.PkgPath == pkg || g.PkgDeps[t.PkgPath][pkg] {
				n++
			}
		}
		return n
	}
	baselineCache := map[string]int{}

	ratios := make([]float64, 0, len(keys))
	var sumSel, sumBase int
	var savings []float64
	for _, k := range keys {
		// Run the real selector over a synthetic hunk on this function rather
		// than querying reachability. They are not the same: the selector adds
		// the fallbacks, and estimating from the internal under-reports
		// wherever one fires. gin's reflectively dispatched methods made the
		// two answers differ by a factor of thirty.
		res := selection.Select(g, []gitdiff.Hunk{
			{File: k.File, StartLine: k.Line, EndLine: k.Line},
		}, selection.Options{})
		sel := len(res.Selected)
		ratios = append(ratios, float64(sel)/float64(total))
		if sel == 0 {
			continue // nothing reaches it; it would tell us nothing about saving
		}
		pkg := k.Pkg
		base, ok := baselineCache[pkg]
		if !ok {
			base = baselineFor(pkg)
			baselineCache[pkg] = base
		}
		if base == 0 {
			continue
		}
		sumSel += sel
		sumBase += base
		savings = append(savings, float64(base-sel)/float64(base))
	}
	sort.Float64s(ratios)
	sort.Float64s(savings)
	med := ratios[len(ratios)/2]
	p10 := ratios[len(ratios)/10]
	p90pick := ratios[min(int(float64(len(ratios))*0.9), len(ratios)-1)]

	fmt.Printf("sampled %d functions; a change to one selects:\n", len(ratios))
	fmt.Printf("  p10 %.1f%%   median %.1f%%   p90 %.1f%%   of the suite\n\n",
		100*p10, 100*med, 100*p90pick)

	if len(savings) > 0 {
		fmt.Printf("against `go test` with a warm cache, over %d reachable functions:\n", len(savings))
		fmt.Printf("  it would run %d tests, whichtests %d — %.0f%% fewer\n",
			sumBase, sumSel, 100*float64(sumBase-sumSel)/float64(sumBase))
		fmt.Printf("  median saving on a single change: %.0f%%\n\n", 100*savings[len(savings)/2])
	}

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

// sampleSymbols picks real source functions uniformly at random.
//
// An earlier version walked packages round-robin to stop one huge package
// dominating, which introduced the opposite and worse bias: cobra has two
// packages, so its small one got half the sample despite holding a tenth of
// the code, and the estimated saving came out far too high. Uniform sampling
// models "a change lands on some function" without weighting by package.
type sampledFunc struct {
	Key  string
	File string
	Line int
	Pkg  string
}

func sampleSymbols(g *graph.Graph, n int, seed int64) []sampledFunc {
	seen := map[string]bool{}
	var all []sampledFunc
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
			all = append(all, sampledFunc{Key: sp.Key, File: file, Line: sp.StartLine, Pkg: sp.PkgPath})
		}
	}
	// deterministic order before a seeded shuffle
	sort.Slice(all, func(i, j int) bool { return all[i].Key < all[j].Key })
	rng := rand.New(rand.NewSource(seed))
	rng.Shuffle(len(all), func(i, j int) { all[i], all[j] = all[j], all[i] })
	if len(all) > n {
		all = all[:n]
	}
	return all
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
