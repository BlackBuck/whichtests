package store

import "testing"

func TestPut(t *testing.T) {
	s := New()
	s.Put("a", "1")
	if len(s.data) != 1 {
		t.Fatal("put failed")
	}
}

func TestGet(t *testing.T) {
	s := New()
	s.Put("a", "1")
	if v, ok := s.Get("a"); !ok || v != "1" {
		t.Fatal("get failed")
	}
}

func TestNormalize(t *testing.T) {
	if Normalize("  A  ") != "a" {
		t.Fatal("normalize failed")
	}
}
