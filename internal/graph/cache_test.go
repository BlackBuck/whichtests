package graph

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

func exampleDir(t *testing.T) string {
	t.Helper()
	dir, err := filepath.Abs("../../example")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Skip("example module not present")
	}
	return dir
}

func reachingNames(g *Graph, key string) []string {
	var out []string
	for tst := range g.Reaching(map[string]bool{key: true}) {
		out = append(out, tst.PkgPath+"."+tst.Name)
	}
	sort.Strings(out)
	return out
}

// TestCacheRoundTrip is the guard that matters: a restored graph must answer
// queries identically to the one that was analysed. A stale or lossy cache
// produces a confidently wrong selection, which is the one failure this tool
// cannot have.
func TestCacheRoundTrip(t *testing.T) {
	t.Setenv("WHICHTESTS_CACHE", t.TempDir())
	dir := exampleDir(t)

	fresh, err := Build(Config{Dir: dir, Cache: true})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.CacheHit {
		t.Fatal("first build should not have hit an empty cache")
	}

	cached, err := Build(Config{Dir: dir, Cache: true})
	if err != nil {
		t.Fatal(err)
	}
	if !cached.CacheHit {
		t.Fatal("second build should have hit the cache")
	}

	if len(fresh.Tests) != len(cached.Tests) {
		t.Fatalf("tests: fresh %d, cached %d", len(fresh.Tests), len(cached.Tests))
	}
	for _, key := range []string{
		"example.com/demo/store.Normalize",
		"example.com/demo/store.New",
		"(*example.com/demo/store.Store).Get",
		"example.com/demo/store.NotAThing",
	} {
		a, b := reachingNames(fresh, key), reachingNames(cached, key)
		if len(a) != len(b) {
			t.Errorf("%s: fresh %v, cached %v", key, a, b)
			continue
		}
		for i := range a {
			if a[i] != b[i] {
				t.Errorf("%s: fresh %v, cached %v", key, a, b)
				break
			}
		}
	}

	// The declaration index and file spans must survive too; selection reads
	// them directly.
	if len(fresh.DeclRefs) != len(cached.DeclRefs) {
		t.Errorf("DeclRefs: fresh %d, cached %d", len(fresh.DeclRefs), len(cached.DeclRefs))
	}
	if len(fresh.Spans) != len(cached.Spans) {
		t.Errorf("Spans: fresh %d, cached %d", len(fresh.Spans), len(cached.Spans))
	}
	if !cached.DeclRefs["example.com/demo/store.Key"]["example.com/demo/store.Normalize"] {
		t.Error("cached DeclRefs lost the Key -> Normalize reference")
	}
}

// TestCacheKeyTracksSource guards the invalidation half: editing a source file
// must produce a different key, or a cache hit would serve a stale graph.
func TestCacheKeyTracksSource(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module m\n\ngo 1.24\n")
	write("a.go", "package m\n\nfunc A() {}\n")

	first, err := CacheKey(dir, []string{"./..."})
	if err != nil {
		t.Fatal(err)
	}
	if again, _ := CacheKey(dir, []string{"./..."}); again != first {
		t.Error("key is not stable across identical inputs")
	}
	if other, _ := CacheKey(dir, []string{"./foo"}); other == first {
		t.Error("patterns must be part of the key")
	}

	write("a.go", "package m\n\nfunc A() { println(1) }\n")
	if changed, _ := CacheKey(dir, []string{"./..."}); changed == first {
		t.Error("editing a source file must change the key")
	}

	write("a.go", "package m\n\nfunc A() {}\n")
	write("go.sum", "example.com/x v1.0.0 h1:abc=\n")
	if changed, _ := CacheKey(dir, []string{"./..."}); changed == first {
		t.Error("a go.sum change must change the key")
	}
}

// TestCacheKeyIncludesLocation guards against serving a snapshot full of
// absolute paths to a checkout that has moved.
func TestCacheKeyIncludesLocation(t *testing.T) {
	build := func() string {
		dir := t.TempDir()
		for name, body := range map[string]string{
			"go.mod": "module m\n\ngo 1.24\n",
			"a.go":   "package m\n\nfunc A() {}\n",
		} {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
		}
		k, err := CacheKey(dir, []string{"./..."})
		if err != nil {
			t.Fatal(err)
		}
		return k
	}
	if build() == build() {
		t.Error("identical sources in different directories must not share a key")
	}
}

func TestTagsAffectBuildAndCacheKey(t *testing.T) {
	if got := buildFlags(nil); got != nil {
		t.Errorf("buildFlags(nil) = %v, want nil", got)
	}
	if got := buildFlags([]string{"acceptance", "integration"}); len(got) != 1 ||
		got[0] != "-tags=acceptance,integration" {
		t.Errorf("buildFlags = %v", got)
	}

	// Tag order must not matter, but the tag set must: loading with a
	// different set produces a different package graph, so the two cannot
	// share a cached snapshot.
	if tagKey([]string{"b", "a"}) != tagKey([]string{"a", "b"}) {
		t.Error("tagKey is order-sensitive")
	}
	if tagKey(nil) == tagKey([]string{"acceptance"}) {
		t.Error("tagged and untagged builds share a cache key")
	}

	dir := t.TempDir()
	for name, body := range map[string]string{
		"go.mod": "module m\n\ngo 1.24\n",
		"a.go":   "package m\n\nfunc A() {}\n",
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	plain, err := CacheKey(dir, []string{"./...", tagKey(nil)})
	if err != nil {
		t.Fatal(err)
	}
	tagged, err := CacheKey(dir, []string{"./...", tagKey([]string{"acceptance"})})
	if err != nil {
		t.Fatal(err)
	}
	if plain == tagged {
		t.Error("cache key ignores build tags")
	}
}
