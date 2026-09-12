package graph

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// TestBuildThroughSymlink guards the path-canonicalization bug: git reports
// symlink-resolved paths while go/packages does not, so if Spans is keyed by
// the unresolved path no diff hunk ever matches and every change silently
// degrades to a full test run.
// TestDeclRefs guards the reverse index that lets a type/const/var change
// resolve to referencing functions instead of dirtying the whole package.
// Without it every such change degrades to exactly what `go test` already does.
func TestDeclRefs(t *testing.T) {
	dir, err := filepath.Abs("../../example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skip("example module not present")
	}
	g, err := Build(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}

	refs := g.DeclRefs["example.com/demo/store.Key"]
	if len(refs) == 0 {
		t.Fatal("no references recorded for type Key")
	}
	if !refs["example.com/demo/store.Normalize"] {
		t.Errorf("Normalize should reference Key, got %v", keysOf(refs))
	}
	if refs["(*example.com/demo/store.Store).Put"] {
		t.Errorf("Put does not mention Key, got %v", keysOf(refs))
	}

	// The Store type is only ever touched through field selection inside its
	// own methods, which ident uses alone would miss.
	store := g.DeclRefs["example.com/demo/store.Store"]
	if !store["(*example.com/demo/store.Store).Get"] {
		t.Errorf("Get selects on Store fields but was not recorded: %v", keysOf(store))
	}

	var found bool
	for _, spans := range g.Decls {
		for _, d := range spans {
			for _, k := range d.Keys {
				if k == "example.com/demo/store.Key" {
					found = true
				}
			}
		}
	}
	if !found {
		t.Error("no DeclSpan recorded for type Key")
	}
}

func keysOf(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestBuildThroughSymlink(t *testing.T) {
	real, err := filepath.Abs("../../example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(real); err != nil {
		t.Skip("example module not present")
	}

	link := filepath.Join(t.TempDir(), "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip("cannot create symlink:", err)
	}

	g, err := Build(Config{Dir: link})
	if err != nil {
		t.Fatal(err)
	}
	if len(g.Tests) != 3 {
		t.Fatalf("got %d tests, want 3", len(g.Tests))
	}
	if len(g.Spans) == 0 {
		t.Fatal("no function spans indexed")
	}
	// Tests:true loads a package several times over and every variant carries
	// the same non-test syntax, so spans must be deduplicated by file.
	for file, spans := range g.Spans {
		seen := map[string]bool{}
		for _, sp := range spans {
			if seen[sp.Key] {
				t.Errorf("duplicate span for %s in %s", sp.Key, file)
			}
			seen[sp.Key] = true
		}
	}
	for file := range g.Spans {
		resolved, err := filepath.EvalSymlinks(file)
		if err != nil {
			t.Errorf("span key %q does not exist on disk", file)
			continue
		}
		if resolved != file {
			t.Errorf("span key %q is not canonical, want %q", file, resolved)
		}
	}

	// TestNormalize must reach Normalize; TestPut must not, even though TestPut
	// calls t.Run and so reaches testing.tRunner, whose indirect t.F() call RTA
	// wires to every func(*testing.T) in the program. Without cutting those
	// cross-test edges this assertion fails and every diff selects 100%.
	reaching := map[string]bool{}
	for tst := range g.Reaching(map[string]bool{"example.com/demo/store.Normalize": true}) {
		reaching[tst.Name] = true
	}
	for _, tc := range []struct {
		test string
		want bool
	}{{"TestNormalize", true}, {"TestPut", false}} {
		if got := reaching[tc.test]; got != tc.want {
			t.Errorf("%s reaches Normalize = %v, want %v (reached: %v)",
				tc.test, got, tc.want, reaching)
		}
	}
}

// TestEveryKeyHasAPackage guards the reflection fallback. A synthesised wrapper
// -- the pointer-receiver shim ssa generates for a value-receiver method -- has
// a nil Pkg, and those are exactly the functions reflection dispatches to. When
// they went unattributed the fallback had no package to mark dirty and silently
// did nothing, so a reflectively-reached change selected zero tests.
func TestEveryKeyHasAPackage(t *testing.T) {
	dir, err := filepath.Abs("../../example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skip("example module not present")
	}
	g, err := Build(Config{Dir: dir})
	if err != nil {
		t.Fatal(err)
	}
	var missing []string
	for key := range g.keys {
		if g.KeyPackage(key) != "" {
			continue
		}
		// A method promoted onto an anonymous struct or interface type has no
		// declaring package and cannot be the target of a source edit, so it
		// needs no attribution.
		if strings.Contains(key, "struct{") || strings.Contains(key, "interface{") {
			continue
		}
		missing = append(missing, key)
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		show := missing
		if len(show) > 10 {
			show = show[:10]
		}
		t.Errorf("%d symbol(s) have no package, e.g. %v", len(missing), show)
	}
}

func TestBoundaries(t *testing.T) {
	// entry -> caller -> target, plus reflective -> target where nothing calls
	// reflective. A walk back from target must report reflective and not entry.
	g := &Graph{
		nodeKey: []string{"p.entry", "p.caller", "p.target", "p.reflective", "p.init"},
		rev: [][]int32{
			nil,    // entry: a test, handled below
			{0},    // caller <- entry
			{1, 3}, // target <- caller, reflective
			nil,    // reflective <- nothing: this is the boundary
			nil,    // init: an entry point, never a boundary
		},
		byKey: map[string][]int32{
			"p.entry": {0}, "p.caller": {1}, "p.target": {2},
			"p.reflective": {3}, "p.init": {4},
		},
		keyPkg: map[string]string{
			"p.entry": "p", "p.caller": "p", "p.target": "p",
			"p.reflective": "p", "p.init": "p",
		},
		testByKey: map[string]*Test{"p.entry": {Name: "TestEntry", PkgPath: "p", Key: "p.entry"}},
	}

	got := g.Boundaries(map[string]bool{"p.target": true})
	if len(got) != 1 || got[0] != "p.reflective" {
		t.Errorf("Boundaries = %v, want [p.reflective]", got)
	}

	// A test entry has no callers by design and must never be reported.
	if b := g.Boundaries(map[string]bool{"p.caller": true}); len(b) != 0 {
		t.Errorf("a walk reaching only a test entry reported %v, want none", b)
	}
	// Neither must a package init.
	if b := g.Boundaries(map[string]bool{"p.init": true}); len(b) != 0 {
		t.Errorf("init reported as a boundary: %v", b)
	}
	// A symbol absent from the graph is dead code, not a boundary.
	if b := g.Boundaries(map[string]bool{"p.missing": true}); len(b) != 0 {
		t.Errorf("unknown symbol reported %v, want none", b)
	}
}
