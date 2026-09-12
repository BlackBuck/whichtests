package main

import (
	"flag"
	"fmt"
	"sort"

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

	fmt.Printf("fattest packages (where selecting within a package pays):\n")
	for i, p := range fattest {
		if i >= 5 || p.n < *fat {
			break
		}
		fmt.Printf("  %5d  %s\n", p.n, p.path)
	}
	if fattest[0].n < *fat {
		fmt.Printf("  none with %d or more tests\n", *fat)
	}

	share := 100 * float64(inFat) / float64(total)
	fmt.Printf("\n%.0f%% of tests live in packages of %d or more.\n", share, *fat)
	fmt.Println()

	switch {
	case share >= 50:
		fmt.Println("Verdict: worth trying on a warm cache. Most of your suite sits in")
		fmt.Println("packages big enough that skipping within one is a real saving.")
	case share >= 20:
		fmt.Println("Verdict: marginal on a warm cache. A minority of your suite is in")
		fmt.Println("packages big enough to benefit, so the gain depends on where your")
		fmt.Println("changes land.")
	default:
		fmt.Println("Verdict: little to gain on a warm cache. `go test` already skips")
		fmt.Println("packages a change cannot affect, and yours are too small for")
		fmt.Println("selecting within one to matter.")
	}

	fmt.Println()
	fmt.Println("Cold CI is a separate question. With nothing cached `go test` runs")
	fmt.Printf("all %d tests no matter how small the diff, so the comparison there is\n", total)
	fmt.Println("against the whole suite rather than against the cache. Persisting")
	fmt.Println("GOCACHE between runs is the cheaper thing to try first.")
	return nil
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
