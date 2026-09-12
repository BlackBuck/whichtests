package selection

import (
	"testing"

	"github.com/BlackBuck/whichtests/internal/gitdiff"
	"github.com/BlackBuck/whichtests/internal/graph"
)

// fixture: package "a" holds Foo (reached by TestFoo) and Bar (reached by
// TestBar); package "b" imports "a" and has TestB.
func fixture() *graph.Graph {
	return &graph.Graph{
		Tests: []*graph.Test{
			{Name: "TestFoo", PkgPath: "a", Reach: map[string]bool{"a.TestFoo": true, "a.Foo": true}},
			{Name: "TestBar", PkgPath: "a", Reach: map[string]bool{"a.TestBar": true, "a.Bar": true}},
			{Name: "TestB", PkgPath: "b", Reach: map[string]bool{"b.TestB": true, "a.Foo": true}},
		},
		Spans: map[string][]graph.FuncSpan{
			"/r/a/a.go": {
				{Key: "a.Foo", PkgPath: "a", StartLine: 10, EndLine: 20},
				{Key: "a.Bar", PkgPath: "a", StartLine: 30, EndLine: 40},
			},
		},
		FilePkg: map[string]string{"/r/a/a.go": "a"},
		PkgDeps: map[string]map[string]bool{
			"a": {},
			"b": {"a": true},
		},
	}
}

func names(r *Result) []string {
	out := make([]string, 0, len(r.Selected))
	for _, s := range r.Selected {
		out = append(out, s.PkgPath+"."+s.Name)
	}
	return out
}

func TestSelect(t *testing.T) {
	cases := []struct {
		name  string
		hunks []gitdiff.Hunk
		opts  Options
		want  []string
	}{
		{
			name:  "symbol hit selects only reaching tests",
			hunks: []gitdiff.Hunk{{File: "/r/a/a.go", StartLine: 12, EndLine: 12}},
			want:  []string{"a.TestFoo", "b.TestB"},
		},
		{
			name:  "unrelated symbol",
			hunks: []gitdiff.Hunk{{File: "/r/a/a.go", StartLine: 35, EndLine: 35}},
			want:  []string{"a.TestBar"},
		},
		{
			name:  "hunk spanning both functions",
			hunks: []gitdiff.Hunk{{File: "/r/a/a.go", StartLine: 15, EndLine: 35}},
			want:  []string{"a.TestBar", "a.TestFoo", "b.TestB"},
		},
		{
			// Between the two funcs: a type, const, or import change. Nothing
			// finer than the package is sound, so every dependent runs.
			name:  "package-level change taints dependents",
			hunks: []gitdiff.Hunk{{File: "/r/a/a.go", StartLine: 25, EndLine: 25}},
			want:  []string{"a.TestBar", "a.TestFoo", "b.TestB"},
		},
		{
			name:  "unknown file is ignored when not in safe mode",
			hunks: []gitdiff.Hunk{{File: "/r/Makefile", StartLine: 1, EndLine: 1}},
			want:  nil,
		},
		{
			name:  "unknown file escalates in safe mode",
			hunks: []gitdiff.Hunk{{File: "/r/Makefile", StartLine: 1, EndLine: 1}},
			opts:  Options{ConservativeOnUnresolved: true},
			want:  []string{"a.TestBar", "a.TestFoo", "b.TestB"},
		},
		{
			name:  "no changes selects nothing",
			hunks: nil,
			want:  nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := names(Select(fixture(), tc.hunks, tc.opts))
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestPlans(t *testing.T) {
	res := Select(fixture(), []gitdiff.Hunk{{File: "/r/a/a.go", StartLine: 12, EndLine: 12}}, Options{})
	plans := res.Plans()
	if len(plans) != 2 {
		t.Fatalf("got %d plans, want 2", len(plans))
	}
	if plans[0].PkgPath != "a" || plans[0].Run != "^(TestFoo)$" {
		t.Errorf("plan[0] = %+v", plans[0])
	}
	if plans[1].PkgPath != "b" || plans[1].Run != "^(TestB)$" {
		t.Errorf("plan[1] = %+v", plans[1])
	}
}

func TestRegexpQuote(t *testing.T) {
	// Go test names legally contain regexp metacharacters once a subtest name
	// is involved; an unescaped one silently changes which tests run.
	if got := regexpQuote("Test_Foo.Bar+1"); got != `Test_Foo\.Bar\+1` {
		t.Errorf("got %q", got)
	}
}

func TestNonBehavioral(t *testing.T) {
	cases := []struct {
		path string
		want bool
	}{
		{"/r/README.md", true},
		{"/r/docs/gh_repo.md", true},
		{"/r/LICENSE", true},
		{"/r/logo.png", true},
		// Golden files are compared against by tests, so an extension alone is
		// never enough to ignore a path.
		{"/r/pkg/cmd/testdata/expected.md", false},
		{"/r/pkg/fixtures/out.png", false},
		{"/r/go.mod", false},
		{"/r/go.sum", false},
		{"/r/script/build.sh", false},
		{"/r/.github/workflows/ci.yml", false},
		{"/r/acceptance/repo.txtar", false},
	}
	for _, c := range cases {
		if got := nonBehavioral(c.path); got != c.want {
			t.Errorf("nonBehavioral(%q) = %v, want %v", c.path, got, c.want)
		}
	}
}

func TestNonBehavioralFilesDoNotEscalate(t *testing.T) {
	g := fixture()
	hunks := []gitdiff.Hunk{{File: "/r/README.md", StartLine: 1, EndLine: 1}}
	opts := Options{ConservativeOnUnresolved: true}

	res := Select(g, hunks, opts)
	if res.Conservative || len(res.Selected) != 0 {
		t.Errorf("doc-only change selected %d tests (conservative=%v), want 0",
			len(res.Selected), res.Conservative)
	}
	if len(res.IgnoredFiles) != 1 {
		t.Errorf("IgnoredFiles = %v, want the README", res.IgnoredFiles)
	}

	// -include-non-behavioral must restore the old escalation for auditing.
	opts.IncludeNonBehavioral = true
	if res := Select(g, hunks, opts); !res.Conservative {
		t.Error("IncludeNonBehavioral should escalate a doc-only change")
	}
}

// declFixture adds a type declaration to the fixture: package "a" declares
// Config at lines 50-55, referenced only by a.Foo.
func declFixture() *graph.Graph {
	g := fixture()
	g.Decls = map[string][]graph.DeclSpan{
		"/r/a/a.go": {
			{Keys: []string{"a.Config"}, PkgPath: "a", StartLine: 50, EndLine: 55},
			{Keys: []string{"a.Orphan"}, PkgPath: "a", StartLine: 60, EndLine: 62},
		},
	}
	g.DeclRefs = map[string]map[string]bool{
		"a.Config": {"a.Foo": true},
		// a.Orphan deliberately has no recorded references.
	}
	return g
}

func TestSelectResolvesDeclarations(t *testing.T) {
	// A change to Config resolves to a.Foo, so only the tests reaching Foo run
	// -- not every test in the package, which is what `go test` already does.
	res := Select(declFixture(), []gitdiff.Hunk{{File: "/r/a/a.go", StartLine: 52, EndLine: 52}}, Options{})
	got := names(res)
	want := []string{"a.TestFoo", "b.TestB"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if len(res.DirtyPackages) != 0 {
		t.Errorf("package should not be dirty, got %v", res.DirtyPackages)
	}
	if len(res.ResolvedDecls) != 1 || res.ResolvedDecls[0] != "a.Config" {
		t.Errorf("ResolvedDecls = %v", res.ResolvedDecls)
	}
}

func TestSelectFallsBackWhenDeclarationHasNoRefs(t *testing.T) {
	// "genuinely unused" and "our index missed the uses" are indistinguishable
	// here, so a declaration with no recorded references must still dirty the
	// package rather than selecting nothing.
	res := Select(declFixture(), []gitdiff.Hunk{{File: "/r/a/a.go", StartLine: 61, EndLine: 61}}, Options{})
	if len(res.DirtyPackages) != 1 || res.DirtyPackages[0] != "a" {
		t.Fatalf("expected package a dirty, got %v", res.DirtyPackages)
	}
	if len(res.Selected) != 3 {
		t.Errorf("expected all 3 tests, got %d", len(res.Selected))
	}
}

func TestSelectFallsBackOutsideAnyDeclaration(t *testing.T) {
	// An import block or a blank line between declarations resolves to nothing.
	res := Select(declFixture(), []gitdiff.Hunk{{File: "/r/a/a.go", StartLine: 5, EndLine: 5}}, Options{})
	if len(res.DirtyPackages) != 1 {
		t.Fatalf("expected the package dirty, got %v", res.DirtyPackages)
	}
}
