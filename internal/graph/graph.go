// Package graph builds the static side of the analysis: an SSA program for the
// module under test, an RTA call graph rooted at every test entry point, and a
// per-test set of reachable function keys.
//
// The unit of reachability is a *function key* (see Key), not a package. That
// is the whole point of whichtests: `go test` already caches at package
// granularity, so a package-level tool duplicates the build cache and buys
// almost nothing. Symbol granularity is what lets us skip 399 of the 400 tests
// in a package when one function changed.
package graph

import (
	"fmt"
	"go/ast"
	"go/token"
	"go/types"
	"strings"

	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/packages"
	"golang.org/x/tools/go/ssa"
	"golang.org/x/tools/go/ssa/ssautil"
)

// Test is a single Test/Benchmark/Fuzz/Example entry point together with the
// set of function keys reachable from it.
type Test struct {
	Name    string // "TestServe"
	PkgPath string // import path to hand to `go test`, with any _test suffix trimmed
	Fn      *ssa.Function
	Reach   map[string]bool
}

// FuncSpan is the line range a top-level function declaration occupies in a
// file, so a diff hunk can be resolved back to the symbol containing it.
type FuncSpan struct {
	Key       string
	PkgPath   string
	StartLine int
	EndLine   int
}

// Graph is the result of a single analysis pass over the module.
type Graph struct {
	Prog  *ssa.Program
	Fset  *token.FileSet
	Tests []*Test

	// Spans is keyed by absolute file path, sorted by StartLine.
	Spans map[string][]FuncSpan
	// FilePkg maps an absolute file path to the import path that owns it.
	FilePkg map[string]string
	// PkgDeps maps an import path to its transitive import closure. Used for
	// the package-level fallback when a hunk lands outside any function body.
	PkgDeps map[string]map[string]bool

	// LoadErrors counts packages that failed to type-check.
	LoadErrors int
}

// Config controls a Build.
type Config struct {
	Dir      string   // module root
	Patterns []string // package patterns, defaults to ./...
}

// Build loads the module, constructs the call graph, and computes per-test
// reachability.
func Build(cfg Config) (*Graph, error) {
	patterns := cfg.Patterns
	if len(patterns) == 0 {
		patterns = []string{"./..."}
	}

	pcfg := &packages.Config{
		Dir:   cfg.Dir,
		Tests: true,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedCompiledGoFiles |
			packages.NeedImports | packages.NeedDeps | packages.NeedTypes |
			packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedModule,
	}
	pkgs, err := packages.Load(pcfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages matched %v", patterns)
	}

	g := &Graph{
		Spans:   make(map[string][]FuncSpan),
		FilePkg: make(map[string]string),
		PkgDeps: make(map[string]map[string]bool),
	}
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		g.LoadErrors += len(p.Errors)
	})

	prog, _ := ssautil.AllPackages(pkgs, ssa.InstantiateGenerics)
	prog.Build()
	g.Prog = prog
	g.Fset = prog.Fset

	var roots []*ssa.Function
	initOf := make(map[*ssa.Package]*ssa.Function)
	seen := make(map[*ssa.Function]bool)

	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		sp := prog.Package(p.Types)
		if sp == nil {
			continue
		}
		g.indexSpans(p, prog)
		g.indexDeps(p)

		if init := sp.Func("init"); init != nil {
			if _, ok := initOf[sp]; !ok {
				initOf[sp] = init
				roots = append(roots, init)
			}
		}
		for _, m := range sp.Members {
			fn, ok := m.(*ssa.Function)
			if !ok || seen[fn] || !isTestEntry(fn) {
				continue
			}
			seen[fn] = true
			g.Tests = append(g.Tests, &Test{
				Name:    fn.Name(),
				PkgPath: runPath(p.PkgPath),
				Fn:      fn,
			})
			roots = append(roots, fn)
		}
	}

	if len(roots) == 0 {
		return g, nil
	}

	cg := rta.Analyze(roots, true).CallGraph
	for _, t := range g.Tests {
		t.Reach = reachable(cg, t.Fn, initOf[t.Fn.Pkg])
	}
	return g, nil
}

// Key is the canonical identity of a symbol, shared by the call-graph side and
// the diff side of the analysis.
//
// Two normalizations matter:
//
//   - Closures collapse into their enclosing top-level function. A diff hunk
//     inside a closure resolves to the enclosing FuncDecl in the AST, so the
//     graph side has to agree or the two never match.
//   - Generic instantiations collapse into their origin, so editing the body
//     of Map[T] matches a test that only ever reaches Map[int].
func Key(fn *ssa.Function) string {
	if fn == nil {
		return ""
	}
	for fn.Parent() != nil {
		fn = fn.Parent()
	}
	if o := fn.Origin(); o != nil {
		fn = o
		for fn.Parent() != nil {
			fn = fn.Parent()
		}
	}
	return fn.String()
}

func reachable(cg *callgraph.Graph, seeds ...*ssa.Function) map[string]bool {
	out := make(map[string]bool)
	visited := make(map[*ssa.Function]bool)
	var queue []*ssa.Function
	for _, s := range seeds {
		if s != nil {
			queue = append(queue, s)
		}
	}
	for len(queue) > 0 {
		fn := queue[0]
		queue = queue[1:]
		if fn == nil || visited[fn] {
			continue
		}
		visited[fn] = true
		out[Key(fn)] = true
		n := cg.Nodes[fn]
		if n == nil {
			continue
		}
		for _, e := range n.Out {
			if e.Callee != nil {
				queue = append(queue, e.Callee.Func)
			}
		}
	}
	return out
}

// indexSpans records the line range of every top-level FuncDecl in the package,
// keyed the same way the call graph keys it. Resolving the *types.Func through
// prog.FuncValue avoids reconstructing ssa's naming scheme by hand.
func (g *Graph) indexSpans(p *packages.Package, prog *ssa.Program) {
	for _, f := range p.Syntax {
		pos := prog.Fset.Position(f.Pos())
		if pos.Filename == "" {
			continue
		}
		g.FilePkg[pos.Filename] = runPath(p.PkgPath)
		for _, d := range f.Decls {
			fd, ok := d.(*ast.FuncDecl)
			if !ok {
				continue
			}
			obj, _ := p.TypesInfo.Defs[fd.Name].(*types.Func)
			if obj == nil {
				continue
			}
			fn := prog.FuncValue(obj)
			if fn == nil {
				continue
			}
			start := prog.Fset.Position(fd.Pos())
			end := prog.Fset.Position(fd.End())
			g.Spans[start.Filename] = append(g.Spans[start.Filename], FuncSpan{
				Key:       Key(fn),
				PkgPath:   runPath(p.PkgPath),
				StartLine: start.Line,
				EndLine:   end.Line,
			})
		}
	}
}

func (g *Graph) indexDeps(p *packages.Package) {
	path := runPath(p.PkgPath)
	deps, ok := g.PkgDeps[path]
	if !ok {
		deps = make(map[string]bool)
		g.PkgDeps[path] = deps
	}
	var walk func(*packages.Package, map[string]bool)
	walk = func(cur *packages.Package, seen map[string]bool) {
		for _, imp := range cur.Imports {
			ip := runPath(imp.PkgPath)
			if seen[ip] {
				continue
			}
			seen[ip] = true
			deps[ip] = true
			walk(imp, seen)
		}
	}
	walk(p, make(map[string]bool))
}

// runPath trims the synthetic _test suffix so external test packages group with
// the package `go test` actually runs.
func runPath(p string) string { return strings.TrimSuffix(p, "_test") }

func isTestEntry(fn *ssa.Function) bool {
	name := fn.Name()
	switch {
	case strings.HasPrefix(name, "Test"),
		strings.HasPrefix(name, "Benchmark"),
		strings.HasPrefix(name, "Fuzz"),
		strings.HasPrefix(name, "Example"):
	default:
		return false
	}
	if fn.Syntax() == nil {
		return false
	}
	sig := fn.Signature
	if sig.Recv() != nil || sig.Results().Len() != 0 {
		return false
	}
	// Examples take no parameters; the rest take exactly one *testing.X.
	if sig.Params().Len() == 0 {
		return strings.HasPrefix(name, "Example")
	}
	if sig.Params().Len() != 1 {
		return false
	}
	ptr, ok := sig.Params().At(0).Type().(*types.Pointer)
	if !ok {
		return false
	}
	named, ok := ptr.Elem().(*types.Named)
	if !ok || named.Obj().Pkg() == nil {
		return false
	}
	return named.Obj().Pkg().Path() == "testing"
}
