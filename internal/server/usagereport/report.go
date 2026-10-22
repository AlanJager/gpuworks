package usagereport

import (
	"encoding/json"
)

// Schema is the version of the report's shape. A change that renames or
// removes a field bumps it; adding a field does not.
const Schema = 1

// Report is one installation's day. Every name in it is either one of the
// platform's own enumerated values, a version, a hardware model, or the ID of
// something public (a built-in template, a public model repository); nothing
// an operator typed is sent. docs/reference/usage-report.md lists every field,
// and a test keeps that list and this type the same.
type Report struct {
	Schema    int    `json:"schema"`
	InstallID string `json:"install_id"`
	// Day is the UTC day (DayLayout) the report covers.
	Day         string      `json:"day"`
	Server      Server      `json:"server"`
	Environment Environment `json:"environment"`
	Usage       Usage       `json:"usage"`
	Tasks       Tasks       `json:"tasks"`
	Errors      Errors      `json:"errors"`
	Delivery    Delivery    `json:"delivery"`
	// Truncated is set when the report had to be cut to fit MaxEncoded.
	Truncated bool `json:"truncated,omitempty"`
}

// Server describes the control plane itself.
type Server struct {
	Version string `json:"version"`
	Channel string `json:"channel"`
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	// SchemaVersion is the database migration the server is at.
	SchemaVersion int `json:"schema_version"`
}

// Environment is what the fleet is made of, as of the moment of the report.
type Environment struct {
	Nodes       int `json:"nodes"`
	OnlineNodes int `json:"online_nodes"`
	// Per node.
	AgentVersions Counts `json:"agent_versions"`
	OS            Counts `json:"os"`
	// Kernels are cut to major.minor.
	Kernels Counts `json:"kernels"`
	Arch    Counts `json:"arch"`
	// Vendors are accelerator vendors (internal/accelerator).
	Vendors Counts `json:"vendors"`
	// Frameworks are "framework major.minor" (cuda 12.4, cann 8.0).
	Frameworks Counts `json:"frameworks"`
	// Drivers are "vendor major" driver versions.
	Drivers  Counts `json:"drivers"`
	Runtimes Counts `json:"runtimes"`
	// ConnectModes are agent_dial and server_dial.
	ConnectModes Counts `json:"connect_modes"`
	// Onboarding is how nodes joined: ssh, cloud or agent (installed by
	// script or by hand).
	Onboarding Counts `json:"onboarding"`
	// Per card.
	GPUModels Counts `json:"gpu_models"`
	// CardOwnership counts cards the platform cannot use, by why
	// (passthrough, no_driver, held_elsewhere).
	CardOwnership Counts `json:"card_ownership"`
	// CardHealth counts cards by health code; healthy cards are not listed.
	CardHealth Counts `json:"card_health"`
	// Providers are the configured cloud provider kinds.
	Providers Counts `json:"providers"`
	// Storages are storage types (local, nfs, nfs_cache, shared_dir).
	Storages Counts `json:"storages"`
}

// Usage is what the installation is used for, as of the moment of the
// report, plus the day's traffic.
type Usage struct {
	Deployments int `json:"deployments"`
	// DeploymentEngines and DeploymentStates count deployments.
	DeploymentEngines Counts `json:"deployment_engines"`
	DeploymentStates  Counts `json:"deployment_states"`
	// MultiNode, PD and Sleepable count deployments with those features;
	// DGPU counts them per dGPU backend.
	MultiNode int    `json:"multi_node"`
	PD        int    `json:"pd"`
	Sleepable int    `json:"sleepable"`
	DGPU      Counts `json:"dgpu"`
	// TemplateSources counts deployments by template source (upstream,
	// user, none); Templates names the built-in templates in use.
	TemplateSources Counts `json:"template_sources"`
	Templates       Counts `json:"templates"`
	// Models names the models deployed: "huggingface:org/name" or
	// "modelscope:org/name" for a public repository, "custom" otherwise.
	Models         Counts `json:"models"`
	InstanceStates Counts `json:"instance_states"`
	// GatewayRequests counts the day's requests through the gateway per
	// deployment engine.
	GatewayRequests Counts `json:"gateway_requests"`
	// Logins counts the day's management sign-ins as ok or failed.
	Logins Counts `json:"logins"`
	// Routes counts the day's management API calls per "METHOD pattern"
	// (an agent upgrade the console asked for is "POST /nodes/{uuid}/upgrade").
	Routes Counts `json:"routes"`
}

// Tasks is what was started during the day and how it went.
type Tasks struct {
	DeploymentsCreated Counts `json:"deployments_created"`
	// InstancesStarted counts instances created during the day; Ready and
	// Failed how many of them reached running or failed by the time of the
	// report; ReadySeconds buckets how long the ready ones took.
	InstancesStarted int    `json:"instances_started"`
	InstancesReady   int    `json:"instances_ready"`
	InstancesFailed  int    `json:"instances_failed"`
	ReadySeconds     Counts `json:"ready_seconds"`
	// ModelFiles counts model files created during the day by
	// "acquisition state" (download present, download failed, peer present...).
	ModelFiles Counts `json:"model_files"`
	// ModelBytes buckets the size of the day's downloaded model files.
	ModelBytes Counts `json:"model_bytes"`
	// Benchmarks counts benchmark runs created during the day by
	// "mode dataset state".
	Benchmarks Counts `json:"benchmarks"`
}

// Errors is where the day went wrong, by code only.
type Errors struct {
	// Events counts the day's deployment timeline events by code.
	Events Counts `json:"events"`
	// APIErrors counts product errors the management API answered by code.
	APIErrors Counts `json:"api_errors"`
	// Panics counts recovered panics by the innermost platform function.
	Panics Counts `json:"panics"`
	// ImagesFailed and ModelFilesFailed count image copies and model files
	// failed right now.
	ImagesFailed     int `json:"images_failed"`
	ModelFilesFailed int `json:"model_files_failed"`
	// Protocol counts agent-link failures by reason.
	Protocol Counts `json:"protocol"`
}

// Delivery is how this report was sent: the endpoint chosen first and how
// long each endpoint took to answer the probe before it.
type Delivery struct {
	Endpoint string `json:"endpoint"`
	// ProbeMS is milliseconds per endpoint kind; -1 when one did not answer.
	ProbeMS Counts `json:"probe_ms"`
}

// Bounds per distribution. Routes and events have more distinct values worth
// seeing than the other distributions.
const (
	maxRouteGroups = 64
	maxEventGroups = 32
)

// MaxEncoded is the largest report sent, uncompressed. A report over it has
// its distributions cut to a few groups each and is marked Truncated.
const MaxEncoded = 128 << 10

// Encode bounds every distribution and returns the report as sent.
func Encode(r *Report) ([]byte, error) {
	bound(r, 1)
	raw, err := json.Marshal(r)
	if err != nil || len(raw) <= MaxEncoded {
		return raw, err
	}
	bound(r, 4)
	r.Truncated = true
	return json.Marshal(r)
}

// bound cuts every distribution; shrink divides the group limits.
func bound(r *Report, shrink int) {
	cut := func(c *Counts, groups int) { *c = Bound(*c, max(1, groups/shrink)) }
	e := &r.Environment
	for _, c := range []*Counts{&e.AgentVersions, &e.OS, &e.Kernels, &e.Arch, &e.Vendors, &e.Frameworks, &e.Drivers,
		&e.Runtimes, &e.ConnectModes, &e.Onboarding, &e.GPUModels, &e.CardOwnership, &e.CardHealth, &e.Providers, &e.Storages} {
		cut(c, MaxGroups)
	}
	u := &r.Usage
	for _, c := range []*Counts{&u.DeploymentEngines, &u.DeploymentStates, &u.DGPU, &u.TemplateSources, &u.Templates,
		&u.Models, &u.InstanceStates, &u.GatewayRequests, &u.Logins} {
		cut(c, MaxGroups)
	}
	cut(&u.Routes, maxRouteGroups)
	t := &r.Tasks
	for _, c := range []*Counts{&t.DeploymentsCreated, &t.ReadySeconds, &t.ModelFiles, &t.ModelBytes, &t.Benchmarks} {
		cut(c, MaxGroups)
	}
	x := &r.Errors
	cut(&x.Events, maxEventGroups)
	cut(&x.APIErrors, maxEventGroups)
	cut(&x.Panics, MaxGroups)
	cut(&x.Protocol, MaxGroups)
}
