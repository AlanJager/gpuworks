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
// derivative of the installation's master key, there only so that the hourly
// checks of one installation count once.
type FleetReport struct {
	InstallID     string
	ServerVersion string
	Nodes         int
	OnlineNodes   int
	// AgentVersions and GPUModels count nodes per agent version and cards per
	// model; the lists are bounded by Query.
	AgentVersions []Count
	GPUModels     []Count
}

// Query encodes the report. Every group is a repeated "name:count" value
// (split at the last colon), so no model name can break the encoding.
func (r *FleetReport) Query() url.Values {
	q := url.Values{}
	q.Set("id", r.InstallID)
	q.Set("v", r.ServerVersion)
	q.Set("nodes", strconv.Itoa(r.Nodes))
	q.Set("online", strconv.Itoa(r.OnlineNodes))
	for _, c := range bounded(r.AgentVersions) {
		q.Add("agent", countParam(c))
	}
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
