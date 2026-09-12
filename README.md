# whichtests

**Symbol-level test impact analysis for Go.** Give it a diff; it tells you which
tests can actually reach the code that changed.

```console
$ whichtests -base main
example.com/demo/store
  TestNormalize

1 of 3 tests selected (33.3%) in 366ms
1 symbol(s) changed, 0 package(s) dirty at package level
```

```console
$ whichtests -base main -format go-test | sh
```

> Status: alpha. The analysis works end to end; the accuracy numbers in
> [Validation](#validation) are not measured yet.

## Why this isn't just the build cache

`go test` already caches results per package, so a *package-level* impact
analyser mostly re-derives what the build cache knows and saves nothing. Every
existing option in this space is package- or coverage-level, and commercial:
Datadog's Test Impact Analysis is coverage-based and SaaS-only; Symflower's
test-runner is package-level and reports ~29% time savings.

whichtests is built around the two places the build cache doesn't help:

1. **Inside a package.** Touch one function in a package with 400 tests and Go
   re-runs all 400. whichtests runs the ones that reach that function.
2. **Cold CI runners.** A fresh container has no build cache at all, so *nothing*
   is skipped, no matter how small the diff.

Both are real. Neither is worth much on most Go repositories. Read the next
section before adopting this.

## When this is worth using

Measured, not assumed. On a real cli/cli commit ([9174ffb0], which changes one
function plus its tests), with the test cache cleared and warmed at the parent
commit:

| | Tests run |
|---|---|
| plain `go test ./...`, warm cache | **21** (4 packages) |
| whichtests | **22** |

It selected *more* than doing nothing clever. That result is structural, not a
bug.

**Go's test cache is keyed on the compiled test binary, not on the source.** It
is already package-level test impact analysis, and it is strictly smarter than
the source-diff kind: a comment change, or any refactor that compiles to an
identical binary, re-runs nothing at all. No diff-based tool can match that.
(Verifying the table above took two attempts for exactly this reason — the first
probe inserted `_ = 4242`, the compiler eliminated it, the binary came back
byte-identical, and `go test` correctly reported every package cached.)

So the only headroom left is selecting *within* a changed package. Whether that
is worth anything depends entirely on one number:

```console
$ for d in $(find . -name '*_test.go' | xargs -n1 dirname | sort -u); do \
    grep -hcE '^func (Test|Benchmark|Fuzz)[A-Z_]' $d/*_test.go; \
  done | sort -n | awk '{a[NR]=$1; s+=$1} END {print "packages:",NR,
      " mean:",s/NR," median:",a[int(NR/2)]," p90:",a[int(NR*0.9)]," max:",a[NR]}'
```

On cli/cli:

```
packages: 250   mean: 7   median: 4   p90: 15   max: 82
```

**The median package has 4 tests.** There is nothing to skip, which is exactly
what 21-vs-22 shows. Go's conventions push toward many small packages, so most
Go repositories look like this.

### Use it when

- **Your packages are fat.** cli/cli's `./api` has 82 tests, `internal/config`
  78, `pkg/cmd/extension` 60. A localized change in one of those makes `go test`
  re-run all of them; whichtests picks a handful. A genuine 10-20x — on 3% of
  the packages.
- **Your suite is slow.** Value is (suite runtime) x (fraction skipped). cli/cli
  runs in minutes, so even perfect selection saves little wall clock. A
  45-minute integration suite is a different proposition.
- **Your CI has no warm cache** — all 1714 tests run against whichtests' 22.
  But the honest competitor here is persisting `GOCACHE` between CI runs: a few
  lines of YAML, zero soundness risk, and it buys the package-level win for
  free. Try that first.

### Don't use it when

Your repository looks like cli/cli: many small packages, a fast suite, and a CI
cache you could just persist. 20 seconds of analysis to select 22 tests where
`go test` already ran 21 for free is a bad trade.

[9174ffb0]: https://github.com/cli/cli/commit/9174ffb0

## How it works

1. **Load** the module with `go/packages` (`Tests: true`) and build SSA.
2. **Root** an RTA pass at every `Test*` / `Benchmark*` / `Fuzz*` / `Example*`
   function plus package `init`s, then refine it with VTA.
3. **Reach** — BFS the call graph from each test, recording only symbols inside
   that test binary's import closure.
4. **Diff** — `git diff --unified=0` from the merge base, including uncommitted
   work, into new-side line ranges.
5. **Resolve** each hunk to the enclosing top-level function, via the FuncDecl
   line spans recorded during loading.
6. **Invert** — select every test whose reachable set intersects the changed
   symbols.

The one design detail that makes steps 3 and 5 agree is the symbol key
(`internal/graph.Key`): closures collapse into their enclosing top-level
function, and generic instantiations collapse into their origin. Without that,
a change inside a closure produces an AST key of `pkg.Outer` while the call
graph only ever has `pkg.Outer$1`, and the two never match.

The second is call graph precision, which is the difference between a useful
tool and a useless one. RTA resolves an indirect call to *every* address-taken
function with a matching signature, so ubiquitous `func()` call sites —
`sync.Once.Do`, defer wrappers, anything reachable from an `init` — become
global hubs. On cli/cli a two-symbol diff selected all 1704 tests because one
closure inside the changed function was linked from every standard library
init. Three fixes, applied in order:

| Fix | That commit |
|---|---|
| RTA only | 1704 / 1704 (100%) |
| + VTA refinement (tracks what actually flows to a call site) | 971 (57%) |
| + import-closure filter | **22 (1.3%)** |

The import-closure filter is the cheap one and the only sound-by-construction
one: a test binary cannot call into a package it does not transitively import,
so any edge leaving that closure is provably false no matter what the call
graph claims. Cross-test contamination is a special case of the same disease —
`testing.tRunner`'s indirect `t.F()` call gets an edge to every
`func(*testing.T)` in the program, so any test calling `t.Run` reaches every
other test. That one is fixed by cutting edges into a *different* test's
subgraph while keeping a test's own subtest closures.

The third is path canonicalization (`internal/fsutil`). `git rev-parse
--show-toplevel` reports a symlink-resolved path and `go/packages` does not, so
on macOS any repo under `/tmp` or `/var` — and any checkout behind a symlinked
workspace directory — produces `/private/var/...` on one side and `/var/...` on
the other. Nothing errors; every file just looks unknown and every diff silently
degrades to a full test run.

## Soundness

Skipping a test that would have caught a bug is the only failure mode that
matters, so the fallbacks are deliberately blunt:

| Situation | Behaviour |
|---|---|
| Hunk lands inside a function body | Select tests reaching that symbol |
| Hunk lands outside any function (type, const, var, import block) | Mark the package dirty; select every test in it and in every package that imports it |
| Hunk touches a file outside the loaded package graph (testdata, fixtures, Makefile, codegen) | `-safe` (default on) escalates to running everything |
| `-conservative` | Run everything, unconditionally |

## Known gaps

These are real and currently unhandled. Each is a reason the tool can under-select.

- **Reflection and `go:linkname`** are invisible to RTA. A test that reaches a
  function only through `reflect.Call` will not be selected.
- **Deleted functions.** A hunk that removes a whole function has no new-side
  span to resolve against, so it falls through to the package-level path. Sound,
  but coarse.
- **`testdata` and golden files** are outside the package graph, so any change to
  them triggers the conservative path under `-safe`.
- **Build tags.** Only the default build configuration is loaded; code behind
  other tags is neither analysed nor selected.
- **Subtests.** Selection is per top-level `Test` function; `-run` cannot narrow
  to a `t.Run` case.
- **Cross-module changes.** Only the module under analysis is diffed.
- **Residual over-selection.** VTA and the import-closure filter remove the
  worst of it, but shared dynamic dispatch *within* a single import closure can
  still link unrelated code. This errs toward running too much, never too
  little.

## Usage

```
whichtests [flags] [packages]

  -base string     git ref to diff against, via merge base with HEAD (default "main")
  -C string        module directory to analyze (default ".")
  -format string   text | json | go-test (default "text")
  -explain         show why each test was selected
  -safe            run everything when the diff touches files outside the package graph (default true)
  -conservative    run everything, unconditionally
```

### In CI

```yaml
- id: select
  run: whichtests -base origin/${{ github.base_ref }} -format json > selection.json
- run: whichtests -base origin/${{ github.base_ref }} -format go-test | sh
```

## Validation

`whichtests-replay` replays a repository's first-parent history and measures the
tool against what actually happened.

```console
# cheap: selection ratio over the last 100 merges, no test execution
$ whichtests-replay -repo ../grafana -n 100

# expensive: run the suite at each commit and check that every failing
# test was selected
$ whichtests-replay -repo ../grafana -n 20 -verify -baseline -json out.json
```

```
replayed 25 commit(s), skipped 0
selection ratio: mean 80.0%, median 100.0%
conservative (ran everything): 17 of 25 commits (68.0%)

what pushed commits onto the conservative path:
  .mod             6 commit(s)
  .sum             6 commit(s)
  .yml             5 commit(s)
  .sh              4 commit(s)
  .txtar           4 commit(s)
  .go              3 commit(s)
```

### Declaration changes resolve to functions

A hunk landing outside any function body -- a type, const, var, or interface --
used to mark the whole package dirty, which is precisely the `go test`
algorithm. Whenever that fired, the tool was by construction no better than
doing nothing. cli/cli commit `75fc31e5` adds one line to an interface:

```go
 type errWithExitCode interface {
+	error
 	ExitCode() int
 }
```

`go test` re-runs 205 packages = 1232 tests. whichtests used to select exactly
1232. It now resolves the declaration to the five functions that reference it:

| | Tests |
|---|---:|
| `go test ./...` | 1232 |
| whichtests, package-dirty fallback | 1232 |
| whichtests, declaration resolution | **872** |

References are collected two ways, and missing either causes under-selection:
naming the identifier, and *selecting* on it (`v.Bar` reads a struct field
without ever writing the type's name). A declaration with no recorded
references still falls back to package-level dirt, because "genuinely unused"
and "our index missed the uses" are indistinguishable from here.

Honest caveat on the aggregate: over 25 merges this moved the mean from 80.0%
to 75.7% and fired on only 4 commits. The package-dirty fallback turned out
*not* to be the dominant one -- the conservative rate stayed at 17 of 25,
because those escalate on files outside the package graph (`go.mod`, `.yml`,
`.sh`, `.txtar`, build-tagged `.go`), which this change does nothing about.

### What 25 cli/cli merges actually look like

The distribution is bimodal, and the mean hides it:

| Commit | Selected |
|---|---|
| docs-only PR | 0 / 1702 |
| docs-only PR | 0 / 1702 |
| validate repo names on create | 22 / 1704 (1.3%) |
| delete the `repo garden` command | 42 / 1713 (2.5%) |
| ssh certificate authority | 885 / 1702 (52%) |
| fix agentic workflow | 1232 / 1702 (72%) |
| invocation telemetry | 1440 / 1715 (84%) |
| issue-13153 plan | 1508 / 1700 (89%) |
| *the other 17* | everything (conservative) |

Half the focused PRs land under 3%. The rest either touch a widely imported
package or trip a fallback. `go.mod`/`go.sum` churn — dependabot merges, mostly
— accounts for 6 of the 17 conservative commits and is genuinely unskippable: a
dependency bump can change anything.

The three `.go` escalations are all `acceptance/*_test.go`, which sits behind a
build tag and so never enters the default package graph. That is the build-tag
gap in [Known gaps](#known-gaps), measured rather than assumed.

Two numbers, deliberately reported together:

- **Selection ratio** is cheap — static analysis only, hundreds of commits in
  minutes. On its own it proves nothing.
- **Recall** is the number that decides whether the tool is safe to adopt: did
  the selected subset still contain every test that failed? It costs one full
  suite run per commit, two with `-baseline`. Budget hours, not minutes.

`-baseline` also runs the suite at the parent commit so that only *newly*
failing tests count. Without it, a test that was already broken before the diff
is scored as a recall violation it had nothing to do with.

A recall violation exits non-zero. It means the tool would have let a real
failure ship, which is a bug, not a statistic.

### Replaying history cannot measure recall

Every commit on a protected trunk passed CI by construction, so `-verify` over
green history finds no failing tests and reports recall as unmeasured. It burns
hours to prove nothing. Use it to validate the pipeline, not to produce the
number.

`whichtests-mutate` produces the number, by manufacturing the failures:

```console
$ whichtests-mutate -C ../cli -n 12
```

For each sampled function it splices `panic(...)` into the top of the body, asks
whichtests which tests it would select had that function changed, runs the whole
suite, and checks that every test which actually failed was inside the
selection. A failing test outside the selection is a recall violation — a real
bug the tool would have let ship. Source is restored after each mutant, and a
mutant nothing catches is reported separately, since it measures the suite's
coverage rather than the tool's selection.

```
injected 4 fault(s), skipped 0
killed by the suite: 4
recall: 4 of 4 killed mutants were caught inside the selection
no recall violations: every failing test was one whichtests would have run
```

### Measured recall on cli/cli

12 faults injected across [cli/cli](https://github.com/cli/cli), one function
at a time, each followed by a full suite run:

```
injected 12 fault(s), skipped 0
killed by the suite: 11
recall: 11 of 11 killed mutants were caught inside the selection
no recall violations: every failing test was one whichtests would have run
```

| Mutated function | Selected | Tests that failed |
|---|---|---|
| `(*extension.Manager).list` | 20 / 1714 | 1 |
| `extension.normalizeExtension` | 28 / 1714 | 1 |
| `pr/view.prLabelList` | 51 / 1714 | 1 |
| `(*queries.ProjectItem).Number` | 62 / 1714 | 2 |
| `attachments.newAttachableMarkdown` | 291 / 1714 | 6 |
| `search.formatAdvancedIssueSearch` | 352 / 1714 | 4 |
| `ghinstance.isGarage` | 949 / 1714 | 7 |
| `(*safeurl.MutableSafeURL).String` | 1426 / 1714 | 86 |

Selection is consistently much larger than the set that actually fails, which
is the correct direction: reaching a function is not the same as asserting on
its behaviour, and the tool only has to avoid *missing* tests.

The twelfth mutant survived — `preview/prompter.runPassword`, which 18 tests
reach statically but none execute. A survived mutant measures the suite's
coverage, not the tool's selection, which is why the two are counted
separately.

Caveats worth stating: 12 mutants is a small sample, and a panic at the top of
a body only exercises selection for changes *inside a function*. It says
nothing about the package-level fallback (a changed type, const, or var) or the
conservative paths, which are validated by unit tests rather than measurement.

```
whichtests-mutate [flags]

  -C string              module directory to analyze (default ".")
  -n int                 number of faults to inject (default 10)
  -seed int              sampling seed, for reproducible runs (default 1)
  -test-timeout duration timeout for one `go test ./...` (default 25m)
  -json string           write the full per-mutant result to this file
  -quiet                 suppress per-mutant progress
```

Replaying uses a detached `git worktree` in a scratch directory, so the
repository under test is never checked out from under you.

```
whichtests-replay [flags]

  -repo string           git repository to replay (default ".")
  -module string         module directory relative to -repo (default ".")
  -rev string            revision to walk back from (default "HEAD")
  -n int                 number of commits to replay (default 50)
  -verify                run the suite at each commit to measure recall (slow)
  -baseline              also run the suite at the parent so only newly failing tests count
  -test-timeout duration timeout for one `go test ./...` (default 20m)
  -json string           write the full per-commit result to this file
  -quiet                 suppress per-commit progress
```

## Performance

Analysis is a full SSA build plus an RTA and a VTA pass, so it scales with the
module, not with the diff. On [cli/cli](https://github.com/cli/cli) (310
packages, 1713 tests) a run takes about 20 seconds on an M-series laptop.

Most of the speedup came from recording less. Traversal still walks through the
standard library — dropping those edges would be unsound — but recording them
stored roughly 3k keys per test for symbols no diff of the module could ever
name. Restricting recording to in-scope packages took a run from 3m47s to
1m10s; VTA's tighter graph took it to 20s.

Caching the reach map keyed by build ID is the next big win: CI currently pays
for RTA on every commit, and the map only changes when the call graph does.

## Roadmap

- [x] SSA + RTA call graph rooted at test entry points
- [x] Per-test reachable symbol sets
- [x] Diff hunk → enclosing symbol resolution
- [x] Package-level and conservative fallbacks
- [x] `text` / `json` / `go-test` output
- [x] Replay harness (`whichtests-replay`) for selection ratio and recall
- [x] VTA refinement and import-closure filtering
- [x] Ignore documentation and assets instead of escalating on them
- [ ] Load build-tag-gated packages (`acceptance/` caused 3 of 25 escalations)
- [ ] Narrow `go.mod`/`go.sum` escalation using the changed modules' reverse deps
- [ ] Reverse the traversal: walk `callgraph.Node.In` from the changed symbols
      instead of building a reach set per test. The forward BFS is 16.5s of a
      20.8s cli/cli run, and it builds 1714 sets to answer a 2-symbol query.
- [ ] `whichtests doctor`: report the tests-per-package distribution so someone
      can tell in 30 seconds whether this repository has any headroom
- [x] Fault injection (`whichtests-mutate`) to measure recall without waiting
      for history to break
- [ ] Cache the reachability map keyed by build ID, so CI pays for RTA once
- [ ] Handle deleted functions by parsing the base revision's AST
- [ ] Subtest granularity
- [ ] GitHub Action wrapper

## Development

```sh
make build   # ./whichtests and ./whichtests-replay
make test
make demo    # run against the bundled example module
make replay  # replay this repo's own history
make mutate  # inject faults into the example module and check recall
```

`example/` is a separate toy module used as a smoke test: three independent
functions with one test each, so symbol-level selection is observable.

## License

MIT
