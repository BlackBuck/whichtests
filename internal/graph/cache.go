package graph

import (
	"crypto/sha256"
	"encoding/gob"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
)

// formatVersion invalidates every cached graph when the analysis changes shape.
// Bump it whenever the snapshot's meaning changes, not just its fields -- a
// stale graph produces a confidently wrong selection, which is the one failure
// mode this tool cannot have.
const formatVersion = "whichtests-graph-v1"

// snapshot is the part of a Graph that answers queries, in a form that survives
// a round trip to disk. The SSA program and the ssa-pointer call graph are not
// in here and are not needed: everything the query touches is keyed by symbol.
type snapshot struct {
	Version string

	Tests   []*Test
	Spans   map[string][]FuncSpan
	Decls   map[string][]DeclSpan
	FilePkg map[string]string

	DeclRefs map[string]map[string]bool
	PkgDeps  map[string]map[string]bool

	// NodeKey and Rev are the call graph. Nodes are already indices, so this
	// serialises directly: Rev[i] holds the callers of node i, and NodeKey[i]
	// the symbol node i is recorded under. keys, byKey and the rest are
	// derived on load rather than stored.
	NodeKey []string
	Rev     [][]int32
	KeyPkg  map[string]string

	LoadErrors int
}

// CacheKey identifies the inputs that determine the graph: every Go source file
// in the module, the module requirements, the toolchain, and the build
// configuration. It deliberately over-approximates -- it hashes .go files that
// no package compiles -- because an unnecessary miss costs four seconds and a
// missed invalidation costs correctness.
//
// Dependency sources are covered by go.sum rather than hashed directly.
func CacheKey(dir string, patterns []string) (string, error) {
	h := sha256.New()
	fmt.Fprintf(h, "%s\n%s\n%s\n%s\n", formatVersion, runtime.Version(),
		runtime.GOOS, runtime.GOARCH)
	// The snapshot stores absolute paths for spans and file ownership, so the
	// module's location is part of its identity. Without this, a checkout moved
	// or copied elsewhere would hit a cache full of paths that no longer exist
	// and silently resolve no diff hunks at all.
	abs, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	fmt.Fprintf(h, "dir:%s\n", abs)
	for _, p := range patterns {
		fmt.Fprintf(h, "pattern:%s\n", p)
	}
	for _, env := range []string{"GOFLAGS", "GOEXPERIMENT", "CGO_ENABLED"} {
		fmt.Fprintf(h, "%s=%s\n", env, os.Getenv(env))
	}

	var files []string
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor":
				return filepath.SkipDir
			}
			return nil
		}
		name := d.Name()
		if strings.HasSuffix(name, ".go") || name == "go.mod" || name == "go.sum" {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	sort.Strings(files)
	for _, f := range files {
		rel, _ := filepath.Rel(dir, f)
		fmt.Fprintf(h, "%s\n", rel)
		if err := hashFile(h, f); err != nil {
			return "", err
		}
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func hashFile(w io.Writer, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(w, f)
	return err
}

// CacheDir is where snapshots live, honouring the usual cache location.
func CacheDir() string {
	if d := os.Getenv("WHICHTESTS_CACHE"); d != "" {
		return d
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return filepath.Join(os.TempDir(), "whichtests")
	}
	return filepath.Join(base, "whichtests")
}

// LoadCached returns the graph stored under key, if one is there.
func LoadCached(key string) (*Graph, bool) {
	f, err := os.Open(filepath.Join(CacheDir(), key+".gob"))
	if err != nil {
		return nil, false
	}
	defer f.Close()

	var snap snapshot
	if err := gob.NewDecoder(f).Decode(&snap); err != nil {
		return nil, false
	}
	if snap.Version != formatVersion {
		return nil, false
	}
	return snap.graph(), true
}

// Save writes the graph under key. A failure to cache is never a failure to
// analyse, so the error is advisory.
func (g *Graph) Save(key string) error {
	dir := CacheDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	// Write to a temporary file and rename, so a concurrent reader never sees
	// a half-written graph.
	tmp, err := os.CreateTemp(dir, "snap-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())

	if err := gob.NewEncoder(tmp).Encode(g.snapshot()); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), filepath.Join(dir, key+".gob"))
}

func (g *Graph) snapshot() *snapshot {
	return &snapshot{
		Version:    formatVersion,
		Tests:      g.Tests,
		Spans:      g.Spans,
		Decls:      g.Decls,
		FilePkg:    g.FilePkg,
		DeclRefs:   g.DeclRefs,
		PkgDeps:    g.PkgDeps,
		NodeKey:    g.nodeKey,
		Rev:        g.rev,
		KeyPkg:     g.keyPkg,
		LoadErrors: g.LoadErrors,
	}
}

func (s *snapshot) graph() *Graph {
	g := &Graph{
		Tests:      s.Tests,
		Spans:      s.Spans,
		Decls:      s.Decls,
		FilePkg:    s.FilePkg,
		DeclRefs:   s.DeclRefs,
		PkgDeps:    s.PkgDeps,
		LoadErrors: s.LoadErrors,
		nodeKey:    s.NodeKey,
		rev:        s.Rev,
		keyPkg:     s.KeyPkg,
		keys:       make(map[string]bool, len(s.KeyPkg)),
		byKey:      make(map[string][]int32, len(s.KeyPkg)),
		testByKey:  make(map[string]*Test, len(s.Tests)),
		indexed:    make(map[string]bool),
	}
	for i, k := range s.NodeKey {
		g.keys[k] = true
		g.byKey[k] = append(g.byKey[k], int32(i))
	}
	for _, t := range s.Tests {
		g.testByKey[t.Key] = t
	}
	g.Reaching = g.reaching
	return g
}

// ErrNoCache reports that nothing was stored for a key.
var ErrNoCache = errors.New("no cached graph")
