package usagereport

import (
	"net/url"
	"strconv"
)

// FleetReport is what an automatic update check tells the publisher about the installation
// that asks: how big it is and what it runs. It travels as query parameters
// of the index request, so the publisher reads it from the access logs of the
// object stores the index is served from (ADR 0040).
//
// It names no host, address, user or deployment. InstallID is a one-way
// derivative of the installation's master key, there so that installations
// can be counted and the hourly checks of one count once.
//
// Every automatic check carries the installation ID, the server version and
// the agent versions; the size of the fleet and its cards only ride along
// when the installation takes part in the improvement program (Fleet).
type FleetReport struct {
	InstallID     string
	ServerVersion string
	// AgentVersions counts nodes per agent version; bounded by Query.
	AgentVersions []Count
	// Fleet adds Nodes, OnlineNodes and GPUModels (cards per model, bounded
	// by Query) to the query.
	Fleet       bool
	Nodes       int
	OnlineNodes int
	GPUModels   []Count
}

// Query encodes the report. Every group is a repeated "name:count" value
// (split at the last colon), so no model name can break the encoding.
func (r *FleetReport) Query() url.Values {
	q := url.Values{}
	q.Set("id", r.InstallID)
	q.Set("v", r.ServerVersion)
	for _, c := range bounded(r.AgentVersions) {
		q.Add("agent", countParam(c))
	}
	if !r.Fleet {
		return q
	}
	q.Set("nodes", strconv.Itoa(r.Nodes))
	q.Set("online", strconv.Itoa(r.OnlineNodes))
	for _, c := range bounded(r.GPUModels) {
		q.Add("gpu", countParam(c))
	}
	return q
}

// countParam is one group as "name:count".
func countParam(c Count) string { return c.Name + ":" + strconv.Itoa(c.N) }

// bounded keeps the largest groups, names cut to MaxName, and
// folds the remainder into one "other" group. The input is not modified.
func bounded(in []Count) []Count { return Bound(in, MaxGroups) }
