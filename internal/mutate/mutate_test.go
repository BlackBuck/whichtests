package mutate

import "testing"

func TestSynthetic(t *testing.T) {
	cases := []struct {
		pkg, key string
		want     bool
	}{
		// The generated test main package and synthesised inits are not source
		// anyone wrote; mutating them edits files the toolchain regenerates.
		{"example.com/demo/store.test", "example.com/demo/store.test.init#1", true},
		{"example.com/demo/store", "example.com/demo/store.init", true},
		{"example.com/demo/store", "example.com/demo/store.init#1", true},
		// Real declarations, including methods and generics, stay mutable.
		{"example.com/demo/store", "example.com/demo/store.Normalize", false},
		{"example.com/demo/store", "(*example.com/demo/store.Store).Get", false},
	}
	for _, c := range cases {
		if got := synthetic(c.pkg, c.key); got != c.want {
			t.Errorf("synthetic(%q, %q) = %v, want %v", c.pkg, c.key, got, c.want)
		}
	}
}
