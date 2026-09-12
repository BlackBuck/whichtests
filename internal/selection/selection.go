// Package selection inverts the reachability map: given the symbols a diff
// touched, which tests can actually reach them.
package selection

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BlackBuck/whichtests/internal/gitdiff"
	"github.com/BlackBuck/whichtests/internal/graph"
)

// Reason records why a test was selected, so `--explain` can justify the
// choice and a skipped test can be defended.
type Reason struct {
	Kind   string // "symbol" | "package" | "conservative"
	Detail string
}

// Selected is one chosen test.
type Selected struct {
	PkgPath string
	Name    string
	Reason  Reason
}

// Result is everything a caller needs to emit a plan or a report.
type Result struct {
	Selected []Selected
	Total    int

	// ChangedSymbols is the set of function keys the diff landed inside.
	ChangedSymbols []string
	// DirtyPackages changed outside any function body (a type, const, var, or
	// import block), which taints every package that depends on them.
	DirtyPackages []string
	// UnresolvedFiles were touched by the diff but belong to no loaded package
	// (testdata, fixtures, Makefiles, generated output). They are the honest
	// reason to keep --conservative around.
	UnresolvedFiles []string
	// Conservative reports that the analysis gave up and selected everything.
	Conservative bool
	// IgnoredFiles were changed but provably cannot affect any test.
	IgnoredFiles []string
	// ResolvedDecls are declaration changes that were resolved to their
	// referencing functions instead of marking the whole package dirty.
	ResolvedDecls []string
}

// resolveDecl maps a hunk that missed every function body to the functions that
// reference the declaration it landed on.
//
// A declaration with no recorded references falls back to package-level dirt on
// purpose: "genuinely unused" and "our index missed the uses" look identical
// from here, and only one of them is safe to act on.
func resolveDecl(g *graph.Graph, h gitdiff.Hunk) ([]string, bool) {
	var out []string
	for _, d := range g.Decls[h.File] {
		if h.StartLine > d.EndLine || h.EndLine < d.StartLine {
			continue
		}
		for _, k := range d.Keys {
			refs := g.DeclRefs[k]
			if len(refs) == 0 {
				return nil, false
			}
			for fn := range refs {
				out = append(out, fn)
			}
		}
	}
	return out, len(out) > 0
}

func declLabel(g *graph.Graph, h gitdiff.Hunk) string {
	for _, d := range g.Decls[h.File] {
		if h.StartLine <= d.EndLine && h.EndLine >= d.StartLine && len(d.Keys) > 0 {
			return d.Keys[0]
		}
	}
	return ""
}

// nonBehavioral reports whether a changed file outside the package graph can be
// ignored rather than escalating to a full run.
//
// This matters more than any call graph refinement: replaying 25 cli/cli
// commits, documentation churn alone forced a full suite run on 9 of them. The
// rule is a narrow allowlist, and it deliberately does not trust an extension
// alone:
//
//   - Anything under testdata/ or fixtures/ is off limits, since a .md or .png
//     there is a golden file a test compares against.
//   - //go:embed targets never reach this check; graph.indexEmbeds already
//     attributes them to the package that embeds them, so changing an embedded
//     README marks that package dirty instead.
func nonBehavioral(path string) bool {
	for _, seg := range strings.Split(filepath.ToSlash(path), "/") {
		if seg == "testdata" || seg == "fixtures" || seg == "golden" {
			return false
		}
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".md", ".markdown", ".rst", ".txt2", ".png", ".jpg", ".jpeg", ".gif",
		".svg", ".ico", ".webp", ".pdf":
		return true
	}
	switch filepath.Base(path) {
	case "LICENSE", "NOTICE", "AUTHORS", "CONTRIBUTORS", "CODEOWNERS",
		".gitignore", ".gitattributes", ".editorconfig":
		return true
	}
	return false
}

// Options controls Select.
type Options struct {
	// Conservative forces selection of every test. Set it from CI when the diff
	// touches anything the analysis cannot model.
	Conservative bool
	// ConservativeOnUnresolved escalates to a full run when the diff touches a
	// file outside the loaded package graph.
	ConservativeOnUnresolved bool
	// IncludeNonBehavioral disables the documentation/asset filter, so every
	// unresolved file escalates. Use it to audit what the filter is skipping.
	IncludeNonBehavioral bool
}

// Select maps hunks to symbols and symbols to tests.
func Select(g *graph.Graph, hunks []gitdiff.Hunk, opts Options) *Result {
	res := &Result{Total: len(g.Tests)}

	changed := make(map[string]bool)
	dirtyPkgs := make(map[string]bool)
	unresolved := make(map[string]bool)
	ignored := make(map[string]bool)
	resolved := make(map[string]bool)

	for _, h := range hunks {
		spans, known := g.Spans[h.File]
		pkg, inPkg := g.FilePkg[h.File]
		if !known && !inPkg {
			if !opts.IncludeNonBehavioral && nonBehavioral(h.File) {
				ignored[h.File] = true
				continue
			}
			unresolved[h.File] = true
			continue
		}
		var hit bool
		for _, s := range spans {
			if h.StartLine <= s.EndLine && h.EndLine >= s.StartLine {
				changed[s.Key] = true
				hit = true
			}
		}
		if hit {
			continue
		}
		// Landed between functions: a type, const, var, or import change.
		// Resolve it to the functions that reference the declaration rather
		// than surrendering the whole package -- package-level dirt is exactly
		// what `go test` already does, so falling back here means the tool
		// bought nothing.
		if keys, ok := resolveDecl(g, h); ok {
			for _, k := range keys {
				changed[k] = true
			}
			resolved[declLabel(g, h)] = true
			continue
		}
		dirtyPkgs[pkg] = true
	}

	res.ChangedSymbols = sortedKeys(changed)
	res.DirtyPackages = sortedKeys(dirtyPkgs)
	res.UnresolvedFiles = sortedKeys(unresolved)
	res.IgnoredFiles = sortedKeys(ignored)
	res.ResolvedDecls = sortedKeys(resolved)

	if opts.Conservative || (opts.ConservativeOnUnresolved && len(unresolved) > 0) {
		res.Conservative = true
		detail := "--conservative"
		if !opts.Conservative {
			detail = fmt.Sprintf("%d file(s) outside the package graph", len(unresolved))
		}
		for _, t := range g.Tests {
			res.Selected = append(res.Selected, Selected{
				PkgPath: t.PkgPath,
				Name:    t.Name,
				Reason:  Reason{Kind: "conservative", Detail: detail},
			})
		}
		sortSelected(res.Selected)
		return res
	}

	for _, t := range g.Tests {
		if r, ok := match(t, changed, dirtyPkgs, g.PkgDeps[t.PkgPath]); ok {
			res.Selected = append(res.Selected, Selected{PkgPath: t.PkgPath, Name: t.Name, Reason: r})
		}
	}
	sortSelected(res.Selected)
	return res
}

func match(t *graph.Test, changed, dirty map[string]bool, deps map[string]bool) (Reason, bool) {
	for key := range t.Reach {
		if changed[key] {
			return Reason{Kind: "symbol", Detail: key}, true
		}
	}
	if dirty[t.PkgPath] {
		return Reason{Kind: "package", Detail: t.PkgPath}, true
	}
	for d := range dirty {
		if deps[d] {
			return Reason{Kind: "package", Detail: d}, true
		}
	}
	return Reason{}, false
}

// Plan groups selected tests into one `go test -run` invocation per package.
type Plan struct {
	PkgPath string
	Run     string
	Tests   []string
}

// Plans renders the selection as runnable commands.
func (r *Result) Plans() []Plan {
	byPkg := make(map[string][]string)
	for _, s := range r.Selected {
		byPkg[s.PkgPath] = append(byPkg[s.PkgPath], s.Name)
	}
	plans := make([]Plan, 0, len(byPkg))
	for pkg, names := range byPkg {
		sort.Strings(names)
		quoted := make([]string, len(names))
		for i, n := range names {
			quoted[i] = regexpQuote(n)
		}
		plans = append(plans, Plan{
			PkgPath: pkg,
			Run:     "^(" + strings.Join(quoted, "|") + ")$",
			Tests:   names,
		})
	}
	sort.Slice(plans, func(i, j int) bool { return plans[i].PkgPath < plans[j].PkgPath })
	return plans
}

// regexpQuote escapes the metacharacters that legally appear in a Go test name.
func regexpQuote(s string) string {
	var b strings.Builder
	for _, r := range s {
		if strings.ContainsRune(`\.+*?()|[]{}^$`, r) {
			b.WriteRune('\\')
		}
		b.WriteRune(r)
	}
	return b.String()
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortSelected(s []Selected) {
	sort.Slice(s, func(i, j int) bool {
		if s[i].PkgPath != s[j].PkgPath {
			return s[i].PkgPath < s[j].PkgPath
		}
		return s[i].Name < s[j].Name
	})
}
