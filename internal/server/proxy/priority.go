package proxy

import (
	"context"
	"net/http"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/priority"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// Request priority (docs/explanation/request-priority.md): the gateway
// decides each request's class from its key's binding and the class the
// client named, and writes the engine's "priority" field for a deployment
// that runs priority scheduling. A client's own field and vLLM's header
// never reach an engine (priority_strip.go).
const (
	// priorityClassHeader names the class a client asks for; the answer
	// carries the class the request ran at under the same name.
	priorityClassHeader = "X-GPUWorks-Priority-Class"
	// priorityClassMaxAge bounds how stale the class snapshot may be when
	// another server changed the classes; this server's own changes reload
	// it at once (store.PriorityClassGeneration).
	priorityClassMaxAge = 5 * time.Second
)

// priorityClasses is the gateway's snapshot of the classes. A request reads
// it without a lock or the database; the request that finds it stale
// reloads it while the others keep using the old one.
type priorityClasses struct {
	store  *store.Store
	now    func() time.Time
	snap   atomic.Pointer[prioritySnapshot]
	reload sync.Mutex
}

type prioritySnapshot struct {
	table priority.Table
	gen   uint64
	at    time.Time
}

func (c *priorityClasses) clock() time.Time {
	if c.now == nil {
		return time.Now()
	}
	return c.now()
}

func (c *priorityClasses) fresh(s *prioritySnapshot, gen uint64) bool {
	return s != nil && s.gen == gen && c.clock().Sub(s.at) < priorityClassMaxAge
}

func (c *priorityClasses) get() priority.Table {
	gen := c.store.PriorityClassGeneration()
	cur := c.snap.Load()
	if c.fresh(cur, gen) {
		return cur.table
	}
	if cur != nil && !c.reload.TryLock() {
		// Another request is reloading; this one uses the last snapshot.
		return cur.table
	}
	if cur == nil {
		c.reload.Lock()
	}
	defer c.reload.Unlock()
	if latest := c.snap.Load(); c.fresh(latest, gen) {
		return latest.table
	}
	table, err := c.store.PriorityClassTable()
	if err != nil {
		if latest := c.snap.Load(); latest != nil {
			// Keep serving the last good snapshot; a later request retries.
			return latest.table
		}
		return priority.NewTable(nil, 0)
	}
	c.snap.Store(&prioritySnapshot{table: table, gen: gen, at: c.clock()})
	return table
}

type priorityBindingKey struct{}

// withPriorityBinding carries the request's key binding from authentication
// to dispatch.
func withPriorityBinding(ctx context.Context, binding priority.Binding) context.Context {
	return context.WithValue(ctx, priorityBindingKey{}, binding)
}

// requestPriority decides the request's class: batch requests run at the
// batch class, requests without a key (playground, open access, probes) at
// the default, a key's requests by its binding and the class asked for.
// ok is false when the client named a class that does not exist.
func (p *Proxy) requestPriority(r *http.Request) (priority.Decision, bool) {
	table := p.priority.get()
	asked := r.Header.Get(priorityClassHeader)
	r.Header.Del(priorityClassHeader)
	source := priority.SourceAnonymous
	caller, _ := uxrecord.CallerFrom(r.Context())
	binding, keyed := r.Context().Value(priorityBindingKey{}).(priority.Binding)
	switch {
	case caller.Source == uxrecord.SourceBatch:
		source = priority.SourceBatch
	case caller.Source == uxrecord.SourceAPI && caller.APIKeyID != 0 && keyed:
		source = priority.SourceKey
	}
	return priority.Resolve(table, source, binding, asked)
}

// applyRequestPriority decides the request's class, says it in the answer's
// header, and writes the engine's priority field: the class's value, in the
// engine's direction, when the deployment runs priority scheduling, and the
// neutral 0 otherwise (only rewriting a body that names the field). It
// writes the error itself when it refuses the request.
func (p *Proxy) applyRequestPriority(w http.ResponseWriter, r *http.Request, deploymentID uint) (*http.Request, func(), bool) {
	decision, ok := p.requestPriority(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, "unknown priority class in "+priorityClassHeader)
		return r, func() {}, false
	}
	w.Header().Set(priorityClassHeader, decision.Class.Name)
	r = r.WithContext(context.WithValue(r.Context(), priorityClassKey{}, decision.Class.Name))
	value := 0
	if dep := p.routedDeployment(deploymentID); dep != nil && dep.PriorityScheduling {
		if v, engineHas := deploy.PriorityFieldValue(dep.Engine, decision.Class.Value); engineHas {
			value = v
		}
	}
	if value == 0 {
		cleanup, err := stripClientPriority(r, p.maxBodyBytes)
		if err != nil {
			writeErr(w, statusForExtractError(err), err.Error())
			return r, cleanup, false
		}
		return r, cleanup, true
	}
	r.Header.Del(priorityHeader)
	if !isJSONRequest(r) || r.GetBody == nil {
		return r, func() {}, true
	}
	cleanup, err := setRequestJSONField(r, priorityField, []byte(strconv.Itoa(value)), true, p.maxBodyBytes)
	if err != nil {
		writeErr(w, statusForExtractError(err), err.Error())
		return r, cleanup, false
	}
	return r, cleanup, true
}

type priorityClassKey struct{}

// priorityClassFrom is the class the request ran at ("" before the gateway
// decided one), for its record.
func priorityClassFrom(ctx context.Context) string {
	name, _ := ctx.Value(priorityClassKey{}).(string)
	return name
}

// routedDeployment is the deployment the route table holds under id, nil
// when the table has not caught up with it (the request then runs at the
// neutral level).
func (p *Proxy) routedDeployment(id uint) *store.Deployment {
	if p.routes == nil {
		return nil
	}
	snap := p.routes.current.Load()
	if snap == nil {
		return nil
	}
	if rd, ok := snap.byID[id]; ok && rd != nil {
		return rd.dep
	}
	return nil
}
