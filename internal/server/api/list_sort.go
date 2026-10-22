package api

import (
	"cmp"
	"net/http"
	"slices"
	"strings"
)

// listSort is a paged list's requested order: sort=<key> ascending,
// sort=-<key> descending. An empty key keeps the list's natural order.
type listSort struct {
	key  string
	desc bool
}

// listSorters names the orders one list offers. Each compares two rows
// ascending; rows that compare equal keep their natural order.
type listSorters[R any] map[string]func(a, b R) int

// parse reads the sort parameter. A key the list does not offer is refused
// rather than ignored, as an unknown state filter is.
func (s listSorters[R]) parse(r *http.Request) (listSort, error) {
	raw, ok := listParam(r.URL.Query(), "sort")
	if !ok || raw == "" {
		return listSort{}, nil
	}
	order := listSort{key: strings.TrimPrefix(raw, "-"), desc: strings.HasPrefix(raw, "-")}
	if _, known := s[order.key]; !known {
		return listSort{}, listFilterError("sort", raw)
	}
	return order, nil
}

// apply orders rows in place; it is stable, so ties keep their natural order.
func (s listSorters[R]) apply(rows []R, order listSort) {
	compare := s[order.key]
	if compare == nil {
		return
	}
	slices.SortStableFunc(rows, func(a, b R) int {
		if order.desc {
			return compare(b, a)
		}
		return compare(a, b)
	})
}

// compareText orders names the way a person reads a list: case-insensitive.
func compareText(a, b string) int {
	return cmp.Compare(strings.ToLower(a), strings.ToLower(b))
}
