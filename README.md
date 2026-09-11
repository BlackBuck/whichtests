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

## How it works

1. **Load** the module with `go/packages` (`Tests: true`) and build SSA.
2. **Root** an RTA call graph at every `Test*` / `Benchmark*` / `Fuzz*` /
   `Example*` function, plus package `init`s.
3. **Reach** — BFS the call graph from each test to get its reachable symbol set.
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

The claim worth defending is **recall**: over a long run of real commits, does
the selected subset still catch every failure the full suite catches? The plan
is to replay recent merges of a large Go repo (Grafana, Loki, containerd) and
report, per commit, tests selected as a percentage and whether any failing test
was missed. A selection percentage without a recall number means nothing.

## Roadmap

- [x] SSA + RTA call graph rooted at test entry points
- [x] Per-test reachable symbol sets
- [x] Diff hunk → enclosing symbol resolution
- [x] Package-level and conservative fallbacks
- [x] `text` / `json` / `go-test` output
- [ ] Replay harness for the recall measurement above
- [ ] Cache the reachability map keyed by build ID, so CI pays for RTA once
- [ ] Handle deleted functions by parsing the base revision's AST
- [ ] Subtest granularity
- [ ] GitHub Action wrapper

## Development

```sh
make build   # ./whichtests
make test
make demo    # run against the bundled example module
```

`example/` is a separate toy module used as a smoke test: three independent
functions with one test each, so symbol-level selection is observable.

## License

MIT
