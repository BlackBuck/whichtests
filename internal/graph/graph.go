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

	"github.com/BlackBuck/whichtests/internal/fsutil"
	"golang.org/x/tools/go/callgraph"
	"golang.org/x/tools/go/callgraph/rta"
	"golang.org/x/tools/go/callgraph/vta"
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
	// BodyOffset is the byte offset just past the body's opening brace, or 0
	// for a function with no body. Fault injection splices a panic in there.
	BodyOffset int
}

// DeclSpan is the line range of a top-level type, const, or var declaration.
// A diff hunk landing here used to mark the whole package dirty, which is
// exactly what `go test` already does -- so the tool was no better than doing
// nothing. Resolving the declaration to the functions that reference it turns
// the most common fallback into real selection.
type DeclSpan struct {
	Keys      []string // declaration keys, one per name in the spec
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

	// Decls is keyed by absolute file path, like Spans.
	Decls map[string][]DeclSpan
	// DeclRefs maps a declaration key to the functions that reference it,
	// whether by naming the identifier or by selecting a field or method on it.
	DeclRefs map[string]map[string]bool

	// LoadErrors counts packages that failed to type-check.
	LoadErrors int

	// indexed guards against double-indexing a file. With Tests:true a package
	// is loaded several times over (itself, the internal test variant, the
	// external test package), and every variant carries the same syntax for the
	// non-test files, so an unguarded append duplicates every span.
	indexed map[string]bool
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
			packages.NeedSyntax | packages.NeedTypesInfo | packages.NeedModule |
			packages.NeedEmbedFiles,
	}
	pkgs, err := packages.Load(pcfg, patterns...)
	if err != nil {
		return nil, fmt.Errorf("load packages: %w", err)
	}
	if len(pkgs) == 0 {
		return nil, fmt.Errorf("no packages matched %v", patterns)
	}

	g := &Graph{
		Spans:    make(map[string][]FuncSpan),
		FilePkg:  make(map[string]string),
		PkgDeps:  make(map[string]map[string]bool),
		indexed:  make(map[string]bool),
		Decls:    make(map[string][]DeclSpan),
		DeclRefs: make(map[string]map[string]bool),
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
	inScope := make(map[*ssa.Package]bool)

	for _, p := range pkgs {
		if p.Types == nil {
			continue
		}
		sp := prog.Package(p.Types)
		if sp == nil {
			continue
		}
		inScope[sp] = true
		g.indexEmbeds(p)
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

	// One RTA pass covers every test, which is the only affordable option: a
	// pass per test would be O(tests) full analyses. The price is that RTA's
	// address-taken set is global, so `testing.tRunner`'s indirect `t.F()` call
	// gets an edge to every func(*testing.T) in the program. Any test that
	// calls t.Run then "reaches" every other test, and through them the whole
	// module. Left alone, that makes every diff select 100% of the suite.
	//
	// Cutting edges into a *different* test's subgraph removes exactly that
	// contamination while keeping a test's own subtest closures, which is where
	// the work being measured actually happens.
	// RTA first to bound the program to what the tests can reach, then VTA to
	// refine it. RTA resolves an indirect call to every address-taken function
	// with a matching signature, which makes ubiquitous `func()` call sites
	// (sync.Once.Do, defer wrappers) global hubs: on cli/cli a two-symbol diff
	// selected all 1704 tests because one closure inside the changed function
	// was linked from every init in the standard library. VTA tracks which
	// function values actually flow to a call site, which collapses those hubs.
	rtaRes := rta.Analyze(roots, true)
	reachable := make(map[*ssa.Function]bool, len(rtaRes.Reachable))
	for fn := range rtaRes.Reachable {
		reachable[fn] = true
	}
	r := &reacher{
		cg:      vta.CallGraph(reachable, rtaRes.CallGraph),
		tests:   make(map[*ssa.Function]bool, len(g.Tests)),
		inScope: inScope,
		path:    make(map[*ssa.Package]string),
	}
	for _, t := range g.Tests {
		r.tests[t.Fn] = true
	}
	for _, t := range g.Tests {
		// A test can only reach packages its own test binary imports. Any edge
		// outside that closure is provably false, whatever the call graph says,
		// so this filter is sound by construction and cheap.
		allowed := g.PkgDeps[t.PkgPath]
		t.Reach = r.reachable(t.Fn, allowed, t.PkgPath, initOf[t.Fn.Pkg])
	}
	return g, nil
}

type reacher struct {
	cg    *callgraph.Graph
	tests map[*ssa.Function]bool
	// inScope holds the packages a diff can actually touch. Traversal still
	// walks through the standard library, but recording those keys would store
	// ~3k entries per test for symbols no diff of this module can ever name.
	inScope map[*ssa.Package]bool
	// path caches import paths so the per-function closure check stays cheap.
	path map[*ssa.Package]string
}

func (r *reacher) pkgPath(p *ssa.Package) string {
	if p == nil {
		return ""
	}
	if v, ok := r.path[p]; ok {
		return v
	}
	v := ""
	if p.Pkg != nil {
		v = runPath(p.Pkg.Path())
	}
	r.path[p] = v
	return v
}

// owner returns the test entry point a function belongs to (itself, or the
// test it is a closure inside), or nil if it belongs to no test.
func (r *reacher) owner(fn *ssa.Function) *ssa.Function {
	for fn.Parent() != nil {
		fn = fn.Parent()
	}
	if r.tests[fn] {
		return fn
	}
	return nil
}

// reachable walks the call graph from a test entry point. The package init is
// seeded alongside it because package-level state is built before the test
// runs, so a change there does affect the test.
func (r *reacher) reachable(self *ssa.Function, allowed map[string]bool, own string, also ...*ssa.Function) map[string]bool {
	out := make(map[string]bool)
	visited := make(map[*ssa.Function]bool)
	queue := []*ssa.Function{self}
	for _, s := range also {
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
		if p := pkgOf(fn); r.inScope[p] {
			if path := r.pkgPath(p); path == own || allowed[path] {
				out[Key(fn)] = true
			}
		}
		n := r.cg.Nodes[fn]
		if n == nil {
			continue
		}
		for _, e := range n.Out {
			if e.Callee == nil || e.Callee.Func == nil {
				continue
			}
			if o := r.owner(e.Callee.Func); o != nil && o != self {
				continue
			}
			queue = append(queue, e.Callee.Func)
		}
	}
	return out
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
//
// pkgOf returns the package a function belongs to, following closures out to
// their enclosing declaration.
func pkgOf(fn *ssa.Function) *ssa.Package {
	for fn.Parent() != nil {
		fn = fn.Parent()
	}
	return fn.Pkg
}

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

// indexSpans records, per file, the line range of every top-level function and
// of every top-level type/const/var declaration, plus the reverse index from a
// declaration to the functions that reference it.
//
// Resolving the *types.Func through prog.FuncValue avoids reconstructing ssa's
// naming scheme by hand.
func (g *Graph) indexSpans(p *packages.Package, prog *ssa.Program) {
	initKey := ""
	if sp := prog.Package(p.Types); sp != nil {
		if init := sp.Func("init"); init != nil {
			initKey = Key(init)
		}
	}

	for _, f := range p.Syntax {
		pos := prog.Fset.Position(f.Pos())
		if pos.Filename == "" {
			continue
		}
		canon := fsutil.Canon(pos.Filename)
		g.FilePkg[canon] = runPath(p.PkgPath)
		if g.indexed[canon] {
			continue
		}
		g.indexed[canon] = true

		for _, d := range f.Decls {
			switch decl := d.(type) {
			case *ast.FuncDecl:
				obj, _ := p.TypesInfo.Defs[decl.Name].(*types.Func)
				if obj == nil {
					continue
				}
				fn := prog.FuncValue(obj)
				if fn == nil {
					continue
				}
				start := prog.Fset.Position(decl.Pos())
				end := prog.Fset.Position(decl.End())
				var bodyOffset int
				if decl.Body != nil {
					bodyOffset = prog.Fset.Position(decl.Body.Lbrace).Offset + 1
				}
				g.Spans[canon] = append(g.Spans[canon], FuncSpan{
					Key:        Key(fn),
					PkgPath:    runPath(p.PkgPath),
					StartLine:  start.Line,
					EndLine:    end.Line,
					BodyOffset: bodyOffset,
				})
			case *ast.GenDecl:
				g.indexGenDecl(p, prog, canon, decl)
			}
		}
		g.indexRefs(p, prog, canon, f, initKey)
	}
}

func (g *Graph) indexGenDecl(p *packages.Package, prog *ssa.Program, file string, decl *ast.GenDecl) {
	for _, spec := range decl.Specs {
		var names []*ast.Ident
		switch sp := spec.(type) {
		case *ast.TypeSpec:
			names = []*ast.Ident{sp.Name}
		case *ast.ValueSpec:
			names = sp.Names
		default:
			continue // import specs carry no declaration of our own
		}
		var keys []string
		for _, n := range names {
			if k := declKey(p.TypesInfo.Defs[n]); k != "" {
				keys = append(keys, k)
			}
		}
		if len(keys) == 0 {
			continue
		}
		start := prog.Fset.Position(spec.Pos())
		end := prog.Fset.Position(spec.End())
		g.Decls[file] = append(g.Decls[file], DeclSpan{
			Keys:      keys,
			PkgPath:   runPath(p.PkgPath),
			StartLine: start.Line,
			EndLine:   end.Line,
		})
	}
}

// indexRefs records which function each reference to a declaration sits inside.
//
// Two kinds of reference matter, and missing either causes under-selection:
//
//   - Naming the identifier: a signature mentioning Foo, a composite literal,
//     a conversion.
//   - Selecting on it: `v.Bar` where v's type is Foo. A function can read a
//     struct's field without ever writing the type's name, so ident uses alone
//     would miss it.
//
// A reference outside any function is attributed to the package init, since
// package-level initialisers run before every test in the package.
func (g *Graph) indexRefs(p *packages.Package, prog *ssa.Program, file string, f *ast.File, initKey string) {
	spans := g.Spans[file]
	enclosing := func(n ast.Node) string {
		line := prog.Fset.Position(n.Pos()).Line
		for _, s := range spans {
			if line >= s.StartLine && line <= s.EndLine {
				return s.Key
			}
		}
		return initKey
	}
	add := func(declK, fnK string) {
		if declK == "" || fnK == "" {
			return
		}
		if g.DeclRefs[declK] == nil {
			g.DeclRefs[declK] = make(map[string]bool)
		}
		g.DeclRefs[declK][fnK] = true
	}

	ast.Inspect(f, func(n ast.Node) bool {
		switch node := n.(type) {
		case *ast.SelectorExpr:
			if sel, ok := p.TypesInfo.Selections[node]; ok {
				if named := namedOf(sel.Recv()); named != nil {
					add(declKey(named.Obj()), enclosing(node))
				}
			}
		case *ast.Ident:
			if obj := p.TypesInfo.Uses[node]; obj != nil {
				if _, isFunc := obj.(*types.Func); !isFunc {
					add(declKey(obj), enclosing(node))
				}
			}
		}
		return true
	})
}

// declKey identifies a package-level declaration. It is a string rather than a
// types.Object pointer because loading with Tests:true type-checks a package
// more than once, so the same declaration has several distinct objects.
func declKey(obj types.Object) string {
	if obj == nil || obj.Pkg() == nil {
		return ""
	}
	if obj.Parent() != obj.Pkg().Scope() {
		return "" // local variable, parameter, or field
	}
	return runPath(obj.Pkg().Path()) + "." + obj.Name()
}

func namedOf(t types.Type) *types.Named {
	for {
		switch x := t.(type) {
		case *types.Pointer:
			t = x.Elem()
		case *types.Named:
			return x
		default:
			return nil
		}
	}
}

// indexEmbeds attributes //go:embed targets to the package that embeds them.
// A change to an embedded file really does change what the package does, so it
// must mark that package dirty rather than falling through to the unresolved
// path — and it stops the non-behavioral file filter from ignoring an embedded
// .md or image.
func (g *Graph) indexEmbeds(p *packages.Package) {
	for _, f := range p.EmbedFiles {
		g.FilePkg[fsutil.Canon(f)] = runPath(p.PkgPath)
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
