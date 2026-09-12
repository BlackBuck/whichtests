# whichtests

**Symbol-level test impact analysis for Go.** Give it a diff; it runs only the
tests that can reach the code that changed.

```console
$ whichtests -base main
github.com/cli/cli/v2/pkg/cmd/pr/merge
  TestPrMerge_deleteBranch
  ...

67 of 1714 tests selected (3.9%) in 0.13s
```

```console
$ whichtests -base main -format go-test | sh
```

> Status: alpha. Every number below is measured on
> [cli/cli](https://github.com/cli/cli) — 310 packages, 1714 tests — and
> reproducible with the tools in this repo.

## Cold CI: 6.7x

The case the tool exists for. A CI container has no cache, so `go test` cannot
skip anything. Commit `8fcd6a64`, two Go files changed:

| cold cache | wall clock |
|---|---:|
| `go test ./...` | **153.7s** |
| whichtests (5s analysis + 18s tests) | **23s** |
| whichtests, graph cache warm | **18.1s** |

Recall, measured two ways: **61 of 61** injected faults caught, and a real
trunk breakage caught inside 9 of 1277 tests. See [Validation](#validation).

Before adopting it, read the next section — with a *warm* cache the picture is
very different.

## When this is worth using

`go test` keys its cache on the **compiled test binary**, so it is already
package-level test impact analysis, and a stricter one than any diff-based tool
can be: a comment change, or any refactor that compiles identically, re-runs
nothing. Against that baseline, warm:

| scenario | `go test` | whichtests |
|---|---:|---:|
| leaf package change | 21 | 22 |
| dependency bump | 107 | 95 |
| core package declaration change | 1232 | 872 |
| 11 function-body faults | 6680 | 4548 |

**0–32% better, never dramatically better.** The only headroom left is
selecting *within* a changed package, and one number caps it:

```console
$ for d in $(find . -name '*_test.go' | xargs -n1 dirname | sort -u); do \
    grep -hcE '^func (Test|Benchmark|Fuzz)[A-Z_]' $d/*_test.go | paste -sd+ - | bc; \
  done | sort -n | awk '{a[NR]=$1} END {print "packages:",NR," median:",a[int(NR/2)]," max:",a[NR]}'
packages: 250   median: 4   max: 82
```

cli/cli: 250 packages, **median 4 tests each**. Nothing to skip, which is
exactly what 21-vs-22 shows. Go's conventions push toward many small packages,
so most repositories look like this.

**Use it when** your CI has no warm cache (persisting `GOCACHE` is the cheaper
first move — try that first), your suite is slow enough that a 30% cut is worth
seconds of analysis, or your packages are fat: cli/cli's `./api` has 82 tests,
`internal/config` 78.

**Don't** when your repo looks like cli/cli warm: many small packages, a fast
suite, and a cache you could just persist.

## How it works

1. **Load** the module with `go/packages` (`Tests: true`) and build SSA.
2. **Root** an RTA pass at every `Test*` / `Benchmark*` / `Fuzz*` / `Example*`
   function plus package `init`s, then refine it with VTA.
3. **Diff** — `git diff --unified=0` from the merge base, including uncommitted
   work.
4. **Resolve** each hunk to its enclosing function, or for a type/const/var, to
   the functions that reference it.
5. **Walk backwards** from those symbols to the tests that reach them.

Four details carry most of the weight:

- **Symbol keys** fold closures into their enclosing function and generics into
  their origin, so the AST side and the graph side agree. Without it a change
  inside a closure yields `pkg.Outer` while the graph only has `pkg.Outer$1`.
- **Node identity stays separate from the key.** Collapsing the *graph* to keys
  looks equivalent and is not: "closure C inside F calls G" becomes "F calls
  G", and every caller of F suddenly reaches G. That silently took one commit
  from 67 tests to 92.
- **The import closure is a hard filter.** A test binary cannot call into a
  package it does not transitively import, so any edge leaving that closure is
  false whatever the call graph says. Without it, one two-symbol diff selected
  all 1704 tests.
- **Paths are canonicalised.** `git rev-parse` resolves symlinks and
  `go/packages` does not, so on macOS every `/var` checkout reports two
  different paths and no hunk ever matches.

## Soundness

Skipping a test that would have caught a bug is the only failure that matters,
so the fallbacks are blunt:

| Situation | Behaviour |
|---|---|
| Hunk inside a function body | Select tests reaching that symbol |
| Hunk on a type/const/var | Select tests reaching its referencing functions |
| That declaration has no known references | Mark the package dirty |
| `go.mod`/`go.sum` change | Select tests importing the changed modules |
| `go`/`toolchain` directive change | Run everything |
| File inside a package's directory (testdata, fixtures) | Mark that package dirty |
| File owned by an unanalyzed package | Ignore: no analyzed test can be affected |
| Docs, assets, `.github/` | Ignore |
| Anything else unrecognised | `-safe` (default) runs everything |

## Known gaps

Each is a reason the tool can under-select, which is the only failure mode that
matters.

- **Reflection**, partially. A function nothing appears to call is treated as
  reflectively reached and its package marked dirty — but only when the walk
  finds *no* tests at all. Gating it wider cost 72.8% precision for no measured
  recall. A diff reaching some tests statically and others only reflectively is
  still under-selected.
- **`go:linkname`** is invisible and has no fallback.
- **Deleted functions** have no new-side span, so they fall through to the
  package-level path. Sound, but coarse.
- **Build tags.** Only the default configuration is loaded, so a change behind
  another tag selects nothing: commit `9b6585be` rewrites 845 lines of a
  build-tagged `acceptance_test.go` and whichtests reports 4 tests. Correct for
  the suite being selected from, useless if you expected those covered.
- **Subtests.** Selection is per top-level `Test`. Table-driven cases share a
  reach set, so static analysis cannot separate them at all.
- **Cross-module changes.** Only the module under analysis is diffed.
- **Residual over-selection.** Shared dynamic dispatch within one import
  closure can still link unrelated code. Errs toward running too much.

## Usage

```
whichtests [flags] [packages]

  -base string     git ref to diff against, via merge base with HEAD (default "main")
  -C string        module directory to analyze (default ".")
  -format string   text | json | go-test (default "text")
  -explain         show why each test was selected
  -cache           reuse an on-disk graph when sources are unchanged (default true)
  -safe            run everything when the diff touches unrecognised files (default true)
  -conservative    run everything, unconditionally
  -include-non-behavioral
                   escalate on docs and assets too, to audit what is skipped
```

```yaml
# in CI
- run: whichtests -base origin/${{ github.base_ref }} -format go-test | sh
```

## Validation

Two harnesses ship with the tool. Both are how every number here was produced.

### Selection, over 25 cli/cli merges

`whichtests-replay` walks first-parent history in a detached worktree and
reports what each merge would have selected.

| | mean | median | ran everything |
|---|---:|---:|---:|
| naive: escalate on anything unrecognised | 95.0% | 100% | 20 / 25 |
| ignore docs and assets | 80.0% | 100% | 17 / 25 |
| resolve declarations to referencing functions | 75.7% | 100% | 17 / 25 |
| narrow dependency bumps to importers | 61.0% | 84.0% | 11 / 25 |
| attribute files to their owning package | **17.3%** | **2.2%** | **0 / 25** |

32,405 tests selected down to **7,423**. The last step mattered most, and it was
removing harm rather than adding cleverness: a `.github/workflows/*.yml` edit
changes no Go input, so `go test` re-runs *nothing* while whichtests ran all
1715. Seven of these 25 merges touch no Go file at all and now select zero.

### Recall, by fault injection

Replaying green history cannot measure recall — every merged commit passed, so
there are no failures to miss. `whichtests-mutate` manufactures them: panic one
function, run the suite, check every test that failed was one whichtests would
have run.

```
injected 80 fault(s), skipped 0
killed by the suite: 61
recall: 61 of 61 killed mutants were caught inside the selection
```

Getting there took a real violation that 12 mutants had missed. Mutating
`(DiscussionActor).Export` selected **zero** tests and `view.TestViewRun` failed
anyway: `cmdutil`'s JSON exporter reaches `ExportData` through
`reflect.ValueOf`, so RTA gives it a node but never an incoming edge. A node
with no callers that is not an entry point is the signature of reflection, and
the selector now falls back to its package.

### Recall, against a real breakage

Trunk is not green everywhere. Commits titled "fix failing tests" mean their
parents were broken; walking back from `6c497e74` finds `f8651f5e` breaking
`gist/edit.Test_editRun` with its parent still passing.

```console
$ whichtests-replay -repo ../cli -rev 6c497e74 -n 5 -verify -baseline
```

| commit | selected | newly failing | missed |
|---|---:|---:|---:|
| **`f8651f5e`** | **9 / 1277** | **1** | **0** |

A genuine breakage caught inside 9 of 1277 tests. `-baseline` is what keeps the
two commits after it — where the test still fails but not *newly* — from being
scored for a bug they did not introduce.

```
whichtests-replay -repo DIR [-rev REV] [-n N] [-verify] [-baseline] [-json FILE]
whichtests-mutate  -C DIR [-n N] [-seed N] [-json FILE]
```

## Performance

Analysis is ~4s on cli/cli, or 0.13s from cache.

| | |
|---|---:|
| forward walk: a reach set per test | 21.4s |
| backward walk from the changed symbols | 4.3s |
| restored from the on-disk graph cache | **0.13s** |

The forward walk built 1714 reach sets, retaining 945,338 keys, to answer a
question about two symbols. Walking backwards gave identical selections on all
25 merges.

The cache is keyed by a hash of every `.go` file, `go.mod`, `go.sum`, the
toolchain, and the module's absolute path — the snapshot stores absolute paths,
so a moved checkout must miss. 18MB for cli/cli. `-cache=false` disables it,
`WHICHTESTS_CACHE` relocates it.

## Roadmap

- [ ] Load build-tag-gated packages
- [ ] Subtest granularity for non-table-driven `t.Run`
- [ ] `whichtests doctor`: report tests-per-package so a repo's ceiling is
      visible in 30 seconds
- [ ] GitHub Action wrapper

## Development

```sh
make build   # whichtests, -replay, -mutate
make test
make demo    # run against the bundled example module
make replay  # replay this repo's own history
make mutate  # inject faults and check recall
```

`example/` is a separate toy module used as a smoke test.

## License

MIT
