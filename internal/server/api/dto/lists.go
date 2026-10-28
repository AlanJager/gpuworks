package dto

import "time"

// List is a whole collection: every element, with Total = len(Items). Every
// unpaged collection answers with it rather than a bare array
// (docs/reference/api-conventions.md).
type List[T any] struct {
	Items []T `json:"items"`
	Total int `json:"total"`
}

// NewList wraps items; an empty collection encodes as "items": [].
func NewList[T any](items []T) List[T] {
	if items == nil {
		items = []T{}
	}
	return List[T]{Items: items, Total: len(items)}
}

// CursorPage is one page of a cursor-paged collection, where the next page
// is asked for by a cursor taken from the last item (the event timelines'
// before=<id>) and a total is not counted.
type CursorPage[T any] struct {
	Items []T `json:"items"`
}

// NewCursorPage wraps items; an empty page encodes as "items": [].
func NewCursorPage[T any](items []T) CursorPage[T] {
	if items == nil {
		items = []T{}
	}
	return CursorPage[T]{Items: items}
}

// ListPage is one page of a paged list: the rows the query's filters match,
// in the list's order, cut to the requested page. Total counts every row the
// filters match, not only this page. Every paged list answers with it (GET
// /deployments and GET /nodes with page parameters, GET /inference-templates).
type ListPage[T any] struct {
	Items    []T `json:"items"`
	Page     int `json:"page"`
	PageSize int `json:"page_size"`
	Total    int `json:"total"`
}

// DeploymentPage is one page of GET /api/v1/deployments?page=…, newest first.
type DeploymentPage = ListPage[DeploymentListItem]

// DeploymentListItem is a deployment as a list row shows it. The launch
// configuration, the catalog selection and the instances stay with
// GET /api/v1/deployments/{id}; every field here but preparation has the
// meaning and value it has on Deployment.
type DeploymentListItem struct {
	// Preparation is the first of the deployment's model_progress still being
	// prepared while it is active; the detail has the whole list.
	Preparation      *ModelNodePresence `json:"preparation,omitempty"`
	ID               uint               `json:"id"`
	Name             string             `json:"name"`
	TemplateID       uint               `json:"template_id,omitempty"`
	TemplateDeviated bool               `json:"template_deviated"`
	Engine           string             `json:"engine"`
	Image            string             `json:"image,omitempty"`
	ModelID          uint               `json:"model_id"`
	ModelName        string             `json:"model_name"`
	ServedName       string             `json:"served_name"`
	NodeUUID         string             `json:"node_uuid"`
	NodeName         string             `json:"node_name,omitempty"`
	NodeNames        []string           `json:"node_names,omitempty"`
	DesiredState     string             `json:"desired_state"`
	RuntimeState     string             `json:"runtime_state"`
	// State, AsleepReplicas and SleepLevel have the meaning and the value they
	// have on Deployment: the state to show, how many of the serving replicas
	// sleep, and the sleep level the engines report.
	State          string `json:"state"`
	AsleepReplicas int    `json:"asleep_replicas,omitempty"`
	SleepLevel     int    `json:"sleep_level,omitempty"`
	Sleepable      bool   `json:"sleepable"`
	Replicas       int    `json:"replicas"`
	// Replicas and ReadyReplicas total both roles for PD deployments, and the
	// detail response totals them the same way, so the row and the drawer
	// print one pair of counts rather than two.
	ReadyReplicas *int                  `json:"ready_replicas,omitempty"`
	Health        DeploymentHealth      `json:"health"`
	Operations    []DeploymentOperation `json:"operations"`
	CreatedAt     time.Time             `json:"created_at"`
}

// NodePage is one page of GET /api/v1/nodes?page=…, newest first. Total
// counts the nodes the filters match and All every node. Labels lists every
// user label key=value pair across all nodes, whatever the filters, so a
// label filter can offer them.
type NodePage struct {
	ListPage[Node]
	All    int               `json:"all"`
	Labels []NodeLabelValues `json:"labels"`
}

// NodeLabelValues is one user label key and the values nodes give it, both
// sorted.
type NodeLabelValues struct {
	Key    string   `json:"key"`
	Values []string `json:"values"`
}

// NodeFacets describes the nodes GET /api/v1/nodes/facets matched: how many,
// and the label values and accelerators they carry, each with the number of
// matched nodes carrying it. Forms that pick where a deployment may run show
// these instead of reading every node.
type NodeFacets struct {
	Total int `json:"total"`
	// Labels are the written labels, keys and values sorted.
	Labels []NodeLabelCounts `json:"labels"`
	// SystemLabels are the generated labels except the node identity (every
	// node has its own), with the server's display text.
	SystemLabels []NodeSystemLabelCounts `json:"system_labels"`
	// GPUs are the distinct accelerator models, as the node's cards report
	// them.
	GPUs []NodeGPUCount `json:"gpus"`
}

// NodeLabelCounts is one written label key and the nodes carrying each value.
type NodeLabelCounts struct {
	Key    string       `json:"key"`
	Values []ValueCount `json:"values"`
}

// ValueCount is one label value and how many matched nodes carry it.
type ValueCount struct {
	Value string `json:"value"`
	Nodes int    `json:"nodes"`
}

// NodeSystemLabelCounts is one system label key and the nodes carrying each
// value.
type NodeSystemLabelCounts struct {
	Key    string                  `json:"key"`
	Values []SystemLabelValueCount `json:"values"`
}

// SystemLabelValueCount is a system label value with its display text and
// how many matched nodes carry it.
type SystemLabelValueCount struct {
	Value       string `json:"value"`
	Display     string `json:"display"`
	DisplayCode string `json:"display_code,omitempty"`
	Nodes       int    `json:"nodes"`
}

// NodeGPUCount is one accelerator model - vendor, name and compute
// capability as a card reports them - and how many matched nodes have one.
// Model is the card's gpuworks.ai/gpu-model label value, so the nodes with
// it can be listed with label=gpuworks.ai/gpu-model=<model>.
type NodeGPUCount struct {
	Vendor string `json:"vendor"`
	Name   string `json:"name"`
	CC     string `json:"cc"`
	Model  string `json:"model"`
	Nodes  int    `json:"nodes"`
	// Cards counts the chip's cards on the matched nodes; MemoryBytes is the
	// largest one's memory (one model can ship in several memory sizes).
	Cards       int   `json:"cards"`
	MemoryBytes int64 `json:"memory_bytes"`
}

// DashboardSummary is what the dashboard shows of the fleet, counted on the
// server so the page does not fetch every node and deployment.
type DashboardSummary struct {
	CurrentMetrics DashboardCurrentMetrics `json:"current_metrics"`
	Nodes          DashboardNodes          `json:"nodes"`
	Deployments    DashboardDeployments    `json:"deployments"`
	// ByVendor groups nodes by accelerator vendor, most GPUs first.
	ByVendor []CapacityGroup `json:"by_vendor"`
	// LabelKeys are the user label keys in use, most widely used first.
	LabelKeys []string `json:"label_keys"`
	// LabelKey is the key ByLabel groups by: the requested one while some
	// node carries it, else the most widely used; empty without labels.
	LabelKey string          `json:"label_key"`
	ByLabel  []CapacityGroup `json:"by_label"`
}

// DashboardNodes counts nodes by state and sums their hardware.
type DashboardNodes struct {
	Total        int     `json:"total"`
	Online       int     `json:"online"`
	Offline      int     `json:"offline"`
	Connecting   int     `json:"connecting"`
	GPUs         int     `json:"gpus"`
	UnusableGPUs int     `json:"unusable_gpus"`
	VRAMGiB      float64 `json:"vram_gib"`
	MemoryGiB    float64 `json:"memory_gib"`
}

// DashboardDeployments counts deployments by their effective state.
//
// Total does not equal Running+Warming, and the difference is deliberate: a
// sleeping or asleep deployment serves nothing, so it is counted as running
// only when it can serve, and it is not warming either. Sleeping deployments
// therefore fall into neither bucket and read as another state, exactly as a
// stopped, failed or offline one does. Sleepable counts the deployments that
// could sleep at all, so a client that wants a sleep total reads
// Total-Running-Warming rather than inventing a bucket the wire does not carry.
type DashboardDeployments struct {
	Total   int `json:"total"`
	Running int `json:"running"`
	// Warming are deployments pulling an image or starting.
	Warming   int `json:"warming"`
	Sleepable int `json:"sleepable"`
}

// CapacityGroup is the capacity of the nodes sharing one value. For a label
// group Key is "key=value", and Unlabeled marks the nodes without the key.
type CapacityGroup struct {
	Key          string  `json:"key"`
	Unlabeled    bool    `json:"unlabeled,omitempty"`
	Nodes        int     `json:"nodes"`
	GPUs         int     `json:"gpus"`
	VRAMGiB      float64 `json:"vram_gib"`
	UnusableGPUs int     `json:"unusable_gpus"`
}

// ModelPage is one page of GET /api/v1/models?page=…: the matching models as
// list rows and the summary of every model.
type ModelPage struct {
	ListPage[ModelListItem]
	Summary ModelListSummary `json:"summary"`
}

// ModelListItem is a model as a list row shows it: the model without its
// README and per-location presence (GET /api/v1/models/{id} carries both),
// with the presence summed instead.
type ModelListItem struct {
	Model
	Presence ModelPresenceSummary `json:"presence"`
}

// ModelPresenceSummary sums where a model's files are: ready copies on nodes
// (node caches counted apart) and in shared storages, transfers in flight
// with their mean progress (0-100), and locations that failed, wait for a
// choice or went missing.
type ModelPresenceSummary struct {
	ReadyNodes      int `json:"ready_nodes"`
	CachedNodes     int `json:"cached_nodes"`
	ReadyStorages   int `json:"ready_storages"`
	InFlight        int `json:"in_flight"`
	InFlightPercent int `json:"in_flight_percent"`
	Failed          int `json:"failed"`
}

// ModelListSummary describes every model, whatever the page or filters.
type ModelListSummary struct {
	Registered int     `json:"registered"`
	ReadyFiles int     `json:"ready_files"`
	Sources    int     `json:"sources"`
	WeightGiB  float64 `json:"weight_gib"`
}

// DashboardCurrentMetrics contains fresh measurements from existing online
// hardware. Null means no sample; a measured zero remains zero.
type DashboardCurrentMetrics struct {
	UtilizationPct  *float64 `json:"utilization_pct"`
	TemperatureC    *float64 `json:"temperature_c"`
	PowerWatts      *float64 `json:"power_watts"`
	MemoryUsedBytes *float64 `json:"memory_used_bytes"`
}
