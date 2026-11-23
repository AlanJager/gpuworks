package api

import (
	"errors"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// searchNetworkTargets uses the same effective nodes and attachment fields as
// the drawer. The latest test (potentially megabytes) is irrelevant to search.
func (a *apiHandlers) searchNetworkTargets(w http.ResponseWriter, r *http.Request) {
	params := r.URL.Query()
	kind := params.Get("kind")
	if kind != "nodes" && kind != "attachments" {
		writeProductError(w, http.StatusBadRequest, ErrorFieldInvalid, "kind must be nodes or attachments", map[string]any{"field": "kind", "detail": "expected nodes or attachments"})
		return
	}
	rawID := chi.URLParam(r, "id")
	id := parseUint(rawID)
	management := isManagementNetworkID(rawID)
	if id == 0 && !management {
		writeProductError(w, http.StatusNotFound, ErrorNetworkNotFound, "network not found", nil)
		return
	}
	detail, err := a.searchableNetwork(id)
	if errors.Is(err, store.ErrNotFound) {
		writeProductError(w, http.StatusNotFound, ErrorNetworkNotFound, "network not found", nil)
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, matchNetworkTargets(detail, kind, params.Get("q"), params["role"], params["source"]))
}

// Shared across clients of the same network, bounded across different networks.
// Search-only clients also refresh periodically for writes on another server.
const (
	networkSearchMaxAge   = 5 * time.Second
	networkSearchCapacity = 8
)

type networkSearchCache struct {
	mu      sync.Mutex
	entries map[uint]networkSearchEntry
}
type networkSearchEntry struct {
	detail     dto.NetworkDetail
	generation uint64
	started    time.Time
}

func (a *apiHandlers) networkSearchGeneration() uint64 {
	generation := a.d.Store.FleetFactsGeneration()
	if b := a.d.Store.Bus; b != nil {
		generation += b.Generation("network")
	}
	return generation
}

// remember requires c.mu. An older detail read can complete after a new one.
func (c *networkSearchCache) remember(id uint, entry networkSearchEntry) {
	if c.entries == nil {
		c.entries = make(map[uint]networkSearchEntry)
	}
	if old, ok := c.entries[id]; ok {
		if old.started.After(entry.started) {
			return
		}
	} else if len(c.entries) == networkSearchCapacity {
		var oldestID uint
		var oldest time.Time
		for id, e := range c.entries {
			if oldest.IsZero() || e.started.Before(oldest) {
				oldestID, oldest = id, e.started
			}
		}
		delete(c.entries, oldestID)
	}
	c.entries[id] = entry
}

func (a *apiHandlers) searchableNetwork(id uint) (dto.NetworkDetail, error) {
	c := &a.networkSearch
	c.mu.Lock()
	defer c.mu.Unlock()
	// Capture the generation before reading; a write during a build expires
	// that entry on the next read. Waiters for the same snapshot share the build.
	entry := networkSearchEntry{started: time.Now().UTC(), generation: a.networkSearchGeneration()}
	prev, ok := c.entries[id]
	if ok && a.d.Store.Bus != nil && prev.generation == entry.generation && entry.started.Sub(prev.started) < networkSearchMaxAge {
		return prev.detail, nil
	}
	state, err := a.networks().FreshState()
	if err != nil {
		return dto.NetworkDetail{}, err
	}
	st := networkState{State: state}
	entry.detail, err = st.detailByID(id)
	if err == nil {
		c.remember(id, entry)
	}
	return entry.detail, err
}

func matchNetworkTargets(detail dto.NetworkDetail, kind, query string, roles, sources []string) dto.NetworkTargetMatches {
	q := strings.ToLower(strings.TrimSpace(query))
	matches := func(fields ...string) bool {
		for _, field := range fields {
			if strings.Contains(strings.ToLower(field), q) {
				return true
			}
		}
		return false
	}
	out := dto.NetworkTargetMatches{IDs: []string{}}
	// An explicitly configured interface, IP or HCA can differ from what the
	// node reported. Both belong to the same searchable overview row.
	directMatches := make(map[uint]bool)
	for _, a := range detail.Attachments {
		labelMatch := a.Target == netplan.SourceLabel && matches(a.LabelKey+"="+a.LabelValue)
		if labelMatch || matches(a.NodeName, a.NodeUUID, a.Interface, a.IP) || matches(a.HCAs...) {
			if kind == "attachments" {
				out.IDs = append(out.IDs, strconv.FormatUint(uint64(a.ID), 10))
			} else if a.Target == netplan.SourceNode {
				directMatches[a.ID] = true
			}
		}
	}
	if kind == "attachments" {
		return out
	}
	for _, n := range detail.Nodes {
		if len(sources) > 0 && !slices.Contains(sources, n.Source) {
			continue
		}
		if len(roles) > 0 && !slices.ContainsFunc(n.Roles, func(role string) bool { return slices.Contains(roles, role) }) {
			continue
		}
		hit := directMatches[n.AttachmentID] || matches(n.NodeName, n.NodeUUID, n.IP, n.Interface) ||
			(n.LabelKey != "" && matches(n.LabelKey+"="+n.LabelValue)) ||
			slices.ContainsFunc(n.HCAs, func(rail dto.NetworkRail) bool {
				return matches(rail.Name, rail.IP, rail.NetDev)
			})
		if !hit {
			for key, value := range n.Labels {
				if matches(key + "=" + value) {
					hit = true
					break
				}
			}
		}
		if hit {
			out.IDs = append(out.IDs, n.NodeUUID)
		}
	}
	return out
}
