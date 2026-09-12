package graph

import (
	"os"
	"path/filepath"
	"testing"
)

// TestBuildThroughSymlink guards the path-canonicalization bug: git reports
// symlink-resolved paths while go/packages does not, so if Spans is keyed by
// the unresolved path no diff hunk ever matches and every change silently
// degrades to a full test run.
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
	for _, tc := range []struct {
		test string
		want bool
	}{{"TestNormalize", true}, {"TestPut", false}} {
		var found *Test
		for _, x := range g.Tests {
			if x.Name == tc.test {
				found = x
			}
		}
		if found == nil {
			t.Fatalf("%s not found", tc.test)
		}
		if got := found.Reach["example.com/demo/store.Normalize"]; got != tc.want {
			t.Errorf("%s reaches Normalize = %v, want %v", tc.test, got, tc.want)
		}
	}
}
