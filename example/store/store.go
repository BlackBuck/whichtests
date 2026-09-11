// Package store is a toy package used to smoke-test whichtests: three
// independent functions in one package, each with its own test.
package store

import "strings"

type Store struct{ data map[string]string }

func New() *Store { return &Store{data: map[string]string{}} }

func (s *Store) Put(k, v string) { s.data[k] = v }

func (s *Store) Get(k string) (string, bool) {
	v, ok := s.data[k]
	return v, ok
}

// Normalize is deliberately unreachable from TestPut/TestGet so that editing it
// should select only TestNormalize.
func Normalize(k string) string { return strings.ToLower(strings.TrimSpace(k)) }
