package store

import "sync"

// weightRepoCache holds the catalog's repository index (WeightRepoIndex) with
// the catalog version it was built from.
type weightRepoCache struct {
	mu      sync.Mutex
	built   bool
	version inferenceSpecsVersion
	index   map[string]uint
}

// inferenceSpecsVersion changes whenever a spec row is added, removed or
// updated: a row added raises the count or the highest id, one removed lowers
// the count, and every write through the store stamps updated_at.
type inferenceSpecsVersion struct {
	count      int64
	maxID      uint
	maxUpdated string
}

func (s *Store) inferenceSpecsVersion() (inferenceSpecsVersion, error) {
	var v inferenceSpecsVersion
	err := s.db.Model(&InferenceSpecRow{}).
		Select("COUNT(*), COALESCE(MAX(id), 0), COALESCE(MAX(updated_at), '')").
		Row().Scan(&v.count, &v.maxID, &v.maxUpdated)
	return v, err
}

// WeightRepoIndex returns build's index over every spec's weight sources
// (inferencecatalog.WeightRepoIndex), rebuilt only when the catalog changed
// since the last call. Every launch planned for a model outside the catalog
// asks it, and decoding the whole catalog each time dominated planning at
// fleet scale. The returned map is shared: callers must not modify it.
func (s *Store) WeightRepoIndex(build func([]InferenceSpecWeightSources) map[string]uint) (map[string]uint, error) {
	version, err := s.inferenceSpecsVersion()
	if err != nil {
		return nil, err
	}
	c := &s.weightRepos
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.built && c.version == version {
		return c.index, nil
	}
	specs, err := s.ListInferenceSpecWeightSources()
	if err != nil {
		return nil, err
	}
	c.index, c.version, c.built = build(specs), version, true
	return c.index, nil
}
