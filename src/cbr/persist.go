package cbr

import (
	"compress/gzip"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// Local stores are gob streams, gzip-compressed, behind a small header. Gob
// and gzip are in the standard library, so the package adds no dependency
// to the engine; gob matches fields by name, so adding fields later keeps
// old files loadable. Received aggregates are JSON (see aggregate.go).

const (
	storeMagic   = "IKEMEN-CBR"
	StoreVersion = 1
	maxStoreSize = 512 << 20 // refuse to inflate past this
)

type storeHeader struct {
	Magic   string
	Version int
}

// SaveStore writes a store atomically: a temporary file in the same
// directory is written, synced and renamed over the target.
func SaveStore(path string, s *Store) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".cbr-*")
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		if !ok {
			tmp.Close()
			os.Remove(tmp.Name())
		}
	}()
	zw := gzip.NewWriter(tmp)
	enc := gob.NewEncoder(zw)
	if err := enc.Encode(storeHeader{storeMagic, StoreVersion}); err != nil {
		return err
	}
	live := *s
	live.Cases = make([]*Case, 0, len(s.Cases))
	for _, c := range s.Cases {
		if !c.Removed {
			live.Cases = append(live.Cases, c)
		}
	}
	if err := enc.Encode(&live); err != nil {
		return err
	}
	if err := zw.Close(); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	os.Chmod(path, 0o644)
	ok = true
	return nil
}

// LoadStore reads a store. A missing file yields an empty store.
func LoadStore(path, char string) (*Store, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return NewStore(char), nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("cbr: %s: %w", path, err)
	}
	dec := gob.NewDecoder(io.LimitReader(zr, maxStoreSize))
	var h storeHeader
	if err := dec.Decode(&h); err != nil {
		return nil, fmt.Errorf("cbr: %s: header: %w", path, err)
	}
	if h.Magic != storeMagic {
		return nil, fmt.Errorf("cbr: %s: not a CBR store", path)
	}
	if h.Version > StoreVersion {
		return nil, fmt.Errorf("cbr: %s: version %d is newer than supported %d", path, h.Version, StoreVersion)
	}
	s := &Store{}
	if err := dec.Decode(s); err != nil {
		return nil, fmt.Errorf("cbr: %s: %w", path, err)
	}
	if s.Char == "" {
		s.Char = char
	}
	if s.Reach == nil {
		s.Reach = newReachModel()
	}
	if s.Reach.Bins == nil {
		s.Reach.Bins = map[int64][]ReachBin{}
	}
	if s.Reach.GeoMax == nil {
		s.Reach.GeoMax = map[int64]float32{}
	}
	if s.Trans == nil {
		s.Trans = newTransitionModel()
	}
	if s.Trans.Edges == nil {
		s.Trans.Edges = map[int64]*Edge{}
	}
	if s.Trans.From == nil {
		s.Trans.From = map[int32]int32{}
	}
	if s.Routes == nil {
		s.Routes = map[uint64]*RouteStats{}
	}
	if s.Precedent == nil {
		s.Precedent = map[int32]int32{}
	}
	if s.HumanRoutes == nil {
		s.HumanRoutes = map[uint64]bool{}
	}
	if s.Actions == nil {
		s.Actions = map[uint64]*ActionStat{}
	}
	if s.CmdStates == nil {
		s.CmdStates = map[int32]map[string]float32{}
	}
	if s.Hypotheses == nil {
		s.Hypotheses = map[int32]*Hypothesis{}
	}
	s.index()
	return s, nil
}

// SaveAggregate writes an aggregate as gzip-compressed JSON.
func SaveAggregate(path string, a *Aggregate) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agg-*")
	if err != nil {
		return err
	}
	zw := gzip.NewWriter(tmp)
	if err := json.NewEncoder(zw).Encode(a); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := zw.Close(); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// LoadAggregate reads a gzip-compressed JSON aggregate. A missing file
// yields nil without error.
func LoadAggregate(path string) (*Aggregate, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer f.Close()
	zr, err := gzip.NewReader(f)
	if err != nil {
		return nil, err
	}
	var a Aggregate
	if err := json.NewDecoder(io.LimitReader(zr, maxStoreSize)).Decode(&a); err != nil {
		return nil, fmt.Errorf("cbr: %s: %w", path, err)
	}
	return &a, nil
}

// fileName turns a character key into a safe, unique file name: a readable
// part (the definition file's base name, or the key itself), then a hash of
// the whole key, so keys that read the same once sanitized (names in other
// scripts, characters sharing a name) never share a file.
func fileName(char string) string {
	base := char
	if i := strings.LastIndexAny(base, "/\\"); i >= 0 {
		base = base[i+1:]
	}
	base = strings.TrimSuffix(base, filepath.Ext(base))
	var b strings.Builder
	for _, r := range base {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	h := fnv.New32a()
	h.Write([]byte(char))
	return fmt.Sprintf("%s-%08x", b.String(), h.Sum32())
}

// LocalPath and GlobalPath are where a character's data lives under dir.
func LocalPath(dir, char string) string {
	return filepath.Join(dir, "local", fileName(char)+".cbr")
}

func GlobalPath(dir, char string) string {
	return filepath.Join(dir, "global", fileName(char)+".agg.gz")
}
