package featuregate

import (
	"fmt"
	"sync/atomic"
)

// DynamicSet holds the features declared while the server runs (by the
// plugin packages installed now), beside the static features a Gate is
// built with. Replace swaps the whole set as one snapshot; a Gate reads it
// with one atomic load per lookup, so a key declared or withdrawn at run
// time is answered at once and a lookup still takes no lock and allocates
// nothing.
type DynamicSet struct {
	// reserved are the static keys, which no dynamic declaration may take.
	reserved map[Key]struct{}
	cur      atomic.Pointer[dynamicSnapshot]
}

// dynamicSnapshot is one Replace: the declared features, sorted by key,
// and the Source that answers for them.
type dynamicSnapshot struct {
	src      Source
	features map[Key]*feature
	order    []Key
}

// NewDynamicSet makes an empty set beside the static features a Gate
// will be built with (NewWithDynamic); Replace refuses their keys.
func NewDynamicSet(static []Feature) *DynamicSet {
	d := &DynamicSet{reserved: make(map[Key]struct{}, len(static))}
	for _, f := range static {
		d.reserved[f.Key] = struct{}{}
	}
	return d
}

// Replace makes features, answered by src, the dynamic declarations: keys
// not listed are no longer declared. It refuses, and changes nothing, when
// a key is invalid, listed twice or one of the static keys. A nil src fails
// closed like a Gate without a Source. Concurrent calls each swap a whole
// snapshot; the last one stands.
func (d *DynamicSet) Replace(features []Feature, src Source) error {
	byKey, order, err := declare(features)
	if err != nil {
		return err
	}
	for _, k := range order {
		if _, ok := d.reserved[k]; ok {
			return fmt.Errorf("feature %q is declared statically", k)
		}
	}
	d.cur.Store(&dynamicSnapshot{src: src, features: byKey, order: order})
	return nil
}
