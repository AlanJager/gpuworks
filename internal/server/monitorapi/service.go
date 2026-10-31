// Package monitorapi turns internal/server/monitor's cluster/service
// snapshots into the /monitor wire dto (api/dto): range/step handling,
// diagnosis wiring, chart naming/scaling, event assembly and the small
// display fields (placement, rolled-up state) the monitor package's own
// types do not carry.
//
// It is a leaf: monitor, monitor/rules, api/dto, proto and stdlib only -
// never store, and never internal/server/api itself (see Deps.NotFound and
// Deps.SeriesTooLarge's doc comments for why the app layer injects those
// sentinel error values instead of this package importing api to reference
// them directly). internal/server/api already sits at its api_fanout
// budget ceiling and cannot add either store or monitor as new imports, so
// the actual monitor.ClusterSnapshot/ServiceSnapshot -> dto conversion has
// to live on this side of the api.MonitorService interface rather than in
// api itself; api.go's job stays purely the HTTP contract (range/step/by/fn
// parsing and validation, 404/400 mapping, writing the already-built dto
// value). internal/server/app wires this package's Service into
// api.Deps.Monitor and supplies Deps by adapting the store into monitor's
// plain Inventory/EventRef shapes - store access itself stays in app.
package monitorapi

import (
	"context"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/healthcheck"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// EventRef is one instance_events row's plain-data projection: just enough
// for overviewEvents to derive a capacity-changing marker from, so this
// package never has to import internal/server/store for the concrete
// InstanceEvent type.
type EventRef struct {
	ID           uint
	Kind         string
	Data         string
	InstanceUUID string
	CreatedAtMS  int64
}

// Deps are the collaborators Service needs. The app layer builds all of
// these by adapting the store; this package never reaches into store
// itself.
type Deps struct {
	TSDB tsdb.Store

	// Inventory returns the current deployments/instances/nodes/GPUs.
	Inventory func(ctx context.Context) (monitor.Inventory, error)

	// DeploymentEvents pages one deployment's folded event timeline,
	// newest first - the same contract as store.DeploymentTimeline: before
	// is an exclusive id cursor (0 starts at the newest entry), limit
	// bounds the page.
	DeploymentEvents func(ctx context.Context, deploymentID uint, before uint, limit int) ([]EventRef, error)

	// Now returns the current time; overridable so tests get a fixed
	// clock. A nil Now means time.Now().
	Now func() time.Time

	// NotFound is the exact error value Deployment returns when id names no
	// known deployment. It is injected (rather than this package declaring
	// its own sentinel) so api.go's errors.Is(err, api.ErrMonitorDeploymentNotFound)
	// check keeps working without this package importing internal/server/api
	// - the app layer wires the same value on both sides.
	NotFound error

	// SeriesTooLarge is the exact error value Series returns when a
	// request's projected series or point count would exceed
	// MaxSeriesCount/MaxSeriesPoints, injected for the same reason as
	// NotFound.
	SeriesTooLarge error

	// Health is what the health checker found for a deployment
	// (healthcheck.Checker.Health). Nil reads every deployment as not
	// checked yet.
	Health func(deploymentID uint) healthcheck.Health

	// Requests serves the per-request views (requests.go). Nil means none
	// is wired: the request view answers not found, the distributions are
	// empty.
	Requests RequestSource
	// BadQuery is the API's sentinel for a request query the store refuses,
	// injected like NotFound.
	BadQuery error
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// Service implements api.MonitorService (structurally - this package never
// imports api). Overview/Deployment/Series results are each memoized and
// single-flighted per (range[, id[, step]]) via monitor.Cached, so many
// browsers polling the same view share one computation (plan §3.1/§6).
type Service struct {
	deps Deps

	overview   *monitor.Cached[dto.MonitorOverview]
	deployment *monitor.Cached[dto.MonitorDeployment]
	series     *monitor.Cached[[]dto.MonitorSeries]
}

// New constructs a Service. deps.Now defaults to time.Now when nil.
func New(deps Deps) *Service {
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	return &Service{
		deps:       deps,
		overview:   monitor.NewCached[dto.MonitorOverview](now, monitor.CacheTTL),
		deployment: monitor.NewCached[dto.MonitorDeployment](now, monitor.CacheTTL),
		series:     monitor.NewCached[[]dto.MonitorSeries](now, monitor.CacheTTL),
	}
}

// computeTimeout bounds one shared computation (Overview/Deployment/Series),
// on top of detachedContext's cancellation immunity: without it, a tsdb
// query that hangs (a stuck disk, a lock some other process never releases)
// would leak the goroutine running that computation forever - detaching it
// from the triggering request's cancellation means nothing else was ever
// going to stop it either. Generous relative to the sub-second queries this
// package normally issues, since it exists purely as a backstop, not a
// latency budget any real request should ever approach.
//
// A var, not a const, solely so a test can shorten it to exercise the
// timeout path deterministically without a real 30-second wait; production
// code never assigns to it.
var computeTimeout = 30 * time.Second

// detachedContext returns a context for a compute closure to run under:
// immune to the triggering caller's own cancellation/deadline
// (context.WithoutCancel - see Overview's doc comment for why), but still
// bounded by computeTimeout so a hung backend call cannot leak the
// computation's goroutine forever. The returned cancel func is always safe
// to defer; callers must call it to release the timer.
func detachedContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), computeTimeout)
}

// Overview serves the /monitor/overview snapshot.
//
// The compute closure detaches from ctx (detachedContext) before doing any
// work: this call may end up being the one monitor.Cached.Get chooses to
// lead the computation for every other concurrent caller sharing this cache
// key, and an aborted request (its ctx canceled) must not cancel the
// computation out from under those other waiters. Get's own ctx - used only
// for "give up waiting on someone else's in-flight computation" - is left
// untouched, so a caller that aborts while merely *waiting* still returns
// promptly.
//
// A cached overview may be older than the request (monitor.Cached serves a
// stale one while it refreshes), so Stale is judged again at serve time from
// the as_of it carries. as_of itself stays what the snapshot was built from.
func (s *Service) Overview(ctx context.Context, rng time.Duration) (dto.MonitorOverview, error) {
	out, err := s.overview.Get(ctx, "cluster", "", rng.String(), func(computeCtx context.Context) (dto.MonitorOverview, error) {
		detached, cancel := detachedContext(computeCtx)
		defer cancel()
		return s.buildOverview(detached, rng)
	})
	if err == nil {
		out.Stale = out.Stale || staleAtServe(out.AsOf, s.deps.now())
	}
	return out, err
}

// staleAtServe is monitor.StaleAt for an as_of in the wire's milliseconds
// (0: no sample at all).
func staleAtServe(asOfMS int64, now time.Time) bool {
	var asOf time.Time
	if asOfMS != 0 {
		asOf = time.UnixMilli(asOfMS)
	}
	return monitor.StaleAt(asOf, now)
}

// Deployment serves the /monitor/deployments/{id} snapshot. See Overview's
// doc comment for why compute runs under a detached, timeout-bounded
// context.
func (s *Service) Deployment(ctx context.Context, id uint, rng time.Duration) (dto.MonitorDeployment, error) {
	out, err := s.deployment.Get(ctx, "service", strconv.FormatUint(uint64(id), 10), rng.String(), func(computeCtx context.Context) (dto.MonitorDeployment, error) {
		detached, cancel := detachedContext(computeCtx)
		defer cancel()
		return s.buildDeployment(detached, id, rng)
	})
	if err == nil {
		out.Stale = out.Stale || staleAtServe(out.AsOf, s.deps.now())
	}
	return out, err
}

// Series serves the /monitor/series metric browser. See Overview's doc
// comment for why compute runs under a detached, timeout-bounded context.
func (s *Service) Series(ctx context.Context, metric, by, fn string, rng, step time.Duration) ([]dto.MonitorSeries, error) {
	key := metric + "|" + by + "|" + fn
	return s.series.Get(ctx, "series", key, rng.String()+"|"+step.String(), func(computeCtx context.Context) ([]dto.MonitorSeries, error) {
		detached, cancel := detachedContext(computeCtx)
		defer cancel()
		return s.buildSeries(detached, metric, by, fn, rng, step)
	})
}

// depsFor builds monitor.Deps around an already-loaded inventory and a
// fixed "now", so BuildCluster/BuildService and this package's own
// supplementary queries (the cluster-wide prompt-token series) all see the
// exact same snapshot instead of racing a live store between reads.
// At is the service as of end: its charts, distributions and latency cover
// the range ending there instead of now, for a range the operator picked.
// What the inventory says now (replicas, health) stays current. Answers are
// not cached: a picked range is rarely asked twice, and its key would be
// the end too. A zero end is the service itself.
func (s *Service) At(end time.Time) *Service {
	if end.IsZero() {
		return s
	}
	view := *s
	view.deps.Now = func() time.Time { return end }
	now := func() time.Time { return end }
	// TTL 0: nothing is kept or served stale, every call computes;
	// single-flight still collapses duplicates.
	view.overview = monitor.NewCached[dto.MonitorOverview](now, 0)
	view.deployment = monitor.NewCached[dto.MonitorDeployment](now, 0)
	view.series = monitor.NewCached[[]dto.MonitorSeries](now, 0)
	return &view
}

func (s *Service) depsFor(inv monitor.Inventory, now time.Time) monitor.Deps {
	return monitor.Deps{
		TSDB:      s.deps.TSDB,
		Inventory: func(context.Context) (monitor.Inventory, error) { return inv, nil },
		Now:       func() time.Time { return now },
	}
}

func nodeNameIndex(nodes []monitor.NodeRef) map[string]string {
	out := make(map[string]string, len(nodes))
	for _, n := range nodes {
		out[n.UUID] = n.Name
	}
	return out
}

func instancesByDeployment(instances []monitor.InstanceRef) map[uint][]monitor.InstanceRef {
	out := map[uint][]monitor.InstanceRef{}
	for _, inst := range instances {
		out[inst.DeploymentID] = append(out[inst.DeploymentID], inst)
	}
	return out
}

func findDeployment(deployments []monitor.DeploymentRef, id uint) (monitor.DeploymentRef, bool) {
	for _, d := range deployments {
		if d.ID == id {
			return d, true
		}
	}
	return monitor.DeploymentRef{}, false
}
