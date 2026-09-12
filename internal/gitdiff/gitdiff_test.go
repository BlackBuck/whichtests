package gitdiff

import "testing"

func TestParseNewRange(t *testing.T) {
	cases := []struct {
		line  string
		start int
		count int
	}{
		{"@@ -1,3 +1,4 @@ func Foo() {", 1, 4},
		{"@@ -10 +12 @@", 12, 1},
		{"@@ -5,2 +4,0 @@", 4, 0},
	}
	for _, c := range cases {
		start, count, ok := parseNewRange(c.line)
		if !ok || start != c.start || count != c.count {
			t.Errorf("parseNewRange(%q) = %d,%d,%v want %d,%d,true", c.line, start, count, ok, c.start, c.count)
		}
	}
}

func TestParseHunks(t *testing.T) {
	diff := `diff --git a/a.go b/a.go
--- a/a.go
+++ b/a.go
@@ -3,0 +4,2 @@ func Foo() {
+	x := 1
+	_ = x
diff --git a/b.go b/b.go
--- a/b.go
+++ b/b.go
@@ -9,2 +8,0 @@ func Bar() {
-	old()
-	older()
`
	hunks, err := parse("/repo", diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 2 {
		t.Fatalf("got %d hunks, want 2", len(hunks))
	}
	if hunks[0].File != "/repo/a.go" || hunks[0].StartLine != 4 || hunks[0].EndLine != 5 {
		t.Errorf("addition hunk = %+v", hunks[0])
	}
	// A pure deletion has no new-side lines; it anchors on the following line so
	// it still resolves to the function that contained the removed code.
	if !hunks[1].Deletion || hunks[1].StartLine != 9 || hunks[1].EndLine != 9 {
		t.Errorf("deletion hunk = %+v", hunks[1])
	}
}

func TestParseSkipsDeletedFiles(t *testing.T) {
	diff := `--- a/gone.go
+++ /dev/null
@@ -1,3 +0,0 @@
-package gone
`
	hunks, err := parse("/repo", diff)
	if err != nil {
		t.Fatal(err)
	}
	if len(hunks) != 0 {
		t.Fatalf("got %d hunks, want 0", len(hunks))
	}
}

func TestModuleOfLine(t *testing.T) {
	cases := []struct {
		line string
		mod  string
		ok   bool
	}{
		// go.mod requirement lines, bare and inside a require block.
		{"require github.com/foo/bar v1.2.3", "github.com/foo/bar", true},
		{"\tgithub.com/foo/bar v1.2.3", "github.com/foo/bar", true},
		{"\tgolang.org/x/sys v0.48.0 // indirect", "golang.org/x/sys", true},
		{"replace example.com/a v1.0.0 => example.com/b v1.1.0", "example.com/a", true},
		// go.sum lines, both the archive and the go.mod hash.
		{"github.com/foo/bar v1.2.3 h1:abc=", "github.com/foo/bar", true},
		{"github.com/foo/bar v1.2.3/go.mod h1:abc=", "github.com/foo/bar", true},
		// A language or toolchain bump can affect every package, so it must
		// report ok with an empty module and force the wildcard.
		{"go 1.24", "", true},
		{"toolchain go1.24.2", "", true},
		// Lines carrying no dependency information.
		{"module github.com/cli/cli/v2", "", false},
		{"require (", "", false},
		{")", "", false},
		{"", "", false},
		{"// a comment", "", false},
	}
	for _, c := range cases {
		mod, ok := moduleOfLine(c.line)
		if mod != c.mod || ok != c.ok {
			t.Errorf("moduleOfLine(%q) = (%q,%v), want (%q,%v)", c.line, mod, ok, c.mod, c.ok)
		}
	}
}
