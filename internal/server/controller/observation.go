package controller

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strconv"
	"sync"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/inferenceapi"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An instance's observation (ADR 0039) is resolved from the api.metrics its
// template's spec declares now, not from the deployment's snapshot: it decides
// what the platform collects, never how the instance runs, so a template edit
// - or a server upgrade that changes a built-in template - reaches running
// instances in place, without a restart.

// observationFor resolves how dep's instances are observed. The template's
// current spec is read first; a spec that no longer exists falls back to the
// api contract the deployment's snapshot kept.
func (c *Controller) observationFor(dep *store.Deployment) deploy.Observation {
	return deploy.ResolveObservation(dep.Engine, c.declaredMetrics(dep))
}

func (c *Controller) declaredMetrics(dep *store.Deployment) *inferenceapi.Metrics {
	if dep.TemplateID != 0 {
		raw, err := c.store.TemplateSpecAPIContract(dep.TemplateID, dep.TemplateSpecSource, dep.TemplateSpecName)
		switch {
		case err == nil:
			return contractMetrics([]byte(raw))
		case !errors.Is(err, store.ErrNotFound):
			c.log.Warn("could not read the template spec's api contract; using the deployment snapshot", "dep", dep.ID, "err", err)
		}
	}
	if dep.CatalogSelectionSnapshot == "" {
		return nil
	}
	selection, err := deploy.DecodeCatalogSelection([]byte(dep.CatalogSelectionSnapshot))
	if err != nil {
		return nil
	}
	return contractMetrics(selection.APIContract)
}

func contractMetrics(raw []byte) *inferenceapi.Metrics {
	if len(raw) == 0 {
		return nil
	}
	var contract inferenceapi.Contract
	if json.Unmarshal(raw, &contract) != nil {
		return nil
	}
	return contract.Metrics
}

// observationRefresh coalesces catalog changes into refresh passes: an event
// only records which templates changed and signals the refresher, so a burst
// of imports, or the startup pass over every template, runs as few passes as
// the refresher can take.
type observationRefresh struct {
	mu        sync.Mutex
	all       bool
	templates map[uint]struct{}
	signal    chan struct{}
}

func newObservationRefresh() *observationRefresh {
	return &observationRefresh{templates: map[uint]struct{}{}, signal: make(chan struct{}, 1)}
}

// request records a change to one template row, or to every template when key
// is store.InferenceTemplatesAll.
func (r *observationRefresh) request(key string) {
	r.mu.Lock()
	if key == store.InferenceTemplatesAll {
		r.all = true
	} else if id, err := strconv.ParseUint(key, 10, 64); err == nil {
		r.templates[uint(id)] = struct{}{}
	}
	r.mu.Unlock()
	select {
	case r.signal <- struct{}{}:
	default:
	}
}

// take returns the pending scope: nil templates means every template.
func (r *observationRefresh) take() (templates []uint, pending bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.all {
		r.all = false
		r.templates = map[uint]struct{}{}
		return nil, true
	}
	if len(r.templates) == 0 {
		return nil, false
	}
	templates = make([]uint, 0, len(r.templates))
	for id := range r.templates {
		templates = append(templates, id)
	}
	r.templates = map[uint]struct{}{}
	return templates, true
}

// runObservationRefresh refreshes observations whenever the catalog changed,
// until ctx ends.
func (c *Controller) runObservationRefresh(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-c.observation.signal:
		}
		templates, pending := c.observation.take()
		if !pending {
			continue
		}
		if err := c.refreshObservations(templates); err != nil {
			c.log.Warn("could not refresh instance observations", "err", err)
		}
	}
}

// refreshObservations re-resolves the observation of every instance of the
// deployments created from templates (every deployment when templates is
// nil), stores the ones that changed in one transaction and sends each node
// the new observations of its instances. Headless ranks are not observed and
// stay as they are.
func (c *Controller) refreshObservations(templates []uint) error {
	observed, err := c.store.DeploymentsForObservation(templates)
	if err != nil {
		return err
	}
	var instances []store.Instance
	resolved := make(map[uint]deploy.Observation, len(observed))
	for index := range observed {
		deployment := &observed[index].Deployment
		resolved[deployment.ID] = c.observationFor(deployment)
		instances = append(instances, observed[index].Instances...)
	}
	changed, err := c.store.RewriteObservations(instances, func(instance *store.Instance, spec *proto.InstanceDesiredState) bool {
		if spec.Headless {
			return false
		}
		before := *spec
		resolved[instance.DeploymentID].Apply(spec)
		return !sameObservation(before, *spec)
	})
	if err != nil {
		return err
	}
	c.pushObservations(changed)
	return nil
}

func sameObservation(a, b proto.InstanceDesiredState) bool {
	return a.MetricPath == b.MetricPath && a.MetricPrefix == b.MetricPrefix &&
		reflect.DeepEqual(a.MetricMap, b.MetricMap) && reflect.DeepEqual(a.Observation, b.Observation)
}

// pushObservations sends each node the observations of its changed instances.
// A node that is offline, or an agent that does not hold the instance, gets
// the observation from the stored desired state at its next state.sync.
func (c *Controller) pushObservations(instances []store.Instance) {
	byNode := map[uint][]proto.ObservationUpdate{}
	for index := range instances {
		spec, err := instances[index].DesiredState()
		if err != nil {
			continue
		}
		byNode[instances[index].NodeID] = append(byNode[instances[index].NodeID], proto.ObservationUpdate{
			UUID: spec.UUID, Observation: spec.Observation,
			MetricPath: spec.MetricPath, MetricPrefix: spec.MetricPrefix, MetricMap: spec.MetricMap,
		})
	}
	for nodeID, updates := range byNode {
		node, err := c.store.NodeByID(nodeID)
		if err != nil || !c.hub.Online(node.UUID) {
			continue
		}
		if err := c.hub.PushToNode(node.UUID, proto.StateUpdate{Observations: updates}); err != nil {
			c.log.Debug("could not push instance observations", "node", node.UUID, "err", err)
		}
	}
}
