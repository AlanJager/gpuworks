package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"runtime"
	"sort"
	"strconv"
	"time"

	"gpuworks.ai/gpuworks/internal/server/diagnostics"
	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/logstore"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/uxrecord"
	"gpuworks.ai/gpuworks/internal/server/uxstore"
	"gpuworks.ai/gpuworks/internal/version"
)

// diagnosticsSources is what a diagnostics collection reads of this server.
type diagnosticsSources struct {
	st      *store.Store
	hub     *hub.Hub
	logs    logstore.Store
	ux      *uxstore.Store
	logFile string
}

// Per-collection bounds on the records read from the databases.
const (
	snapshotEventsPerScope   = 5000
	snapshotRequestsPerScope = 10000
)

func (s diagnosticsSources) OpenNodeFiles(ctx context.Context, uuid string, sessions []string) (io.ReadCloser, error) {
	if s.hub == nil {
		return nil, errors.New("the server is not running; nodes cannot be asked")
	}
	return s.hub.OpenDiagnosticsRead(ctx, uuid, sessions)
}

// InstanceLogs pages the instance's lines newest first until max bytes, then
// writes what it kept oldest first: a long log keeps its latest part.
func (s diagnosticsSources) InstanceLogs(ctx context.Context, uuid string, from, to time.Time, w io.Writer, max int64) (bool, error) {
	var kept []logstore.Entry
	var size int64
	filter := logstore.Filter{Instances: []string{uuid}, FromMS: from.UnixMilli(), ToMS: to.UnixMilli(), Limit: logstore.MaxLimit}
	truncated := false
	for size < max {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		page, err := s.logs.Search(filter)
		if err != nil {
			return false, err
		}
		for _, e := range page.Entries {
			line := int64(len(e.Line)) + 48
			if size+line > max {
				truncated = true
				break
			}
			size += line
			kept = append(kept, e)
		}
		if truncated || !page.Truncated || len(page.Entries) == 0 {
			break
		}
		last := page.Entries[len(page.Entries)-1]
		filter.CursorTS, filter.CursorID = last.TimeMS, last.ID
	}
	for i := len(kept) - 1; i >= 0; i-- {
		e := kept[i]
		if _, err := fmt.Fprintf(w, "%s %s %s\n", time.UnixMilli(e.TimeMS).UTC().Format(time.RFC3339Nano), e.Level, e.Line); err != nil {
			return truncated, err
		}
	}
	return truncated, nil
}

func (s diagnosticsSources) InstanceNodes(uuids []string) ([]string, error) {
	var out []string
	for _, uuid := range uuids {
		inst, err := s.st.InstanceByUUID(uuid)
		if err != nil || inst.NodeID == 0 {
			continue
		}
		if node, err := s.st.NodeByID(inst.NodeID); err == nil {
			out = append(out, node.UUID)
		}
	}
	return out, nil
}

func (s diagnosticsSources) DeploymentInstances(ids []uint) ([]string, error) {
	var out []string
	for _, id := range ids {
		instances, err := s.st.InstancesForDeployment(id)
		if err != nil {
			return nil, err
		}
		for _, inst := range instances {
			out = append(out, inst.UUID)
		}
	}
	return out, nil
}

// Snapshot is the records a supporter would otherwise ask for one by one:
// every node (hardware, runtime, versions, state), the chosen deployments and
// instances with their events in the window, the image rows of the chosen
// nodes, storages, settings, and the chosen deployments' request records.
func (s diagnosticsSources) Snapshot(ctx context.Context, scope diagnostics.SnapshotScope) (map[string]any, error) {
	docs := map[string]any{
		"server": map[string]any{"version": version.String(), "go": runtime.Version(), "os": runtime.GOOS, "arch": runtime.GOARCH,
			"window_from": scope.From.UTC().Format(time.RFC3339), "window_to": scope.To.UTC().Format(time.RFC3339)},
	}
	nodes, err := s.st.ListNodes()
	if err != nil {
		return nil, err
	}
	docs["nodes"] = nodes

	deployments := map[uint]bool{}
	for _, id := range scope.DeploymentIDs {
		deployments[id] = true
	}
	instances := []store.Instance{}
	for _, uuid := range scope.InstanceUUIDs {
		if inst, err := s.st.InstanceByUUID(uuid); err == nil {
			instances = append(instances, *inst)
			deployments[inst.DeploymentID] = true
		}
	}
	docs["instances"] = instances
	deps := []store.Deployment{}
	events := []store.InstanceEvent{}
	inWindow := func(e store.InstanceEvent) bool { return !e.CreatedAt.Before(scope.From) && !e.CreatedAt.After(scope.To) }
	depIDs := make([]uint, 0, len(deployments))
	for id := range deployments {
		depIDs = append(depIDs, id)
	}
	sort.Slice(depIDs, func(i, j int) bool { return depIDs[i] < depIDs[j] })
	for _, id := range depIDs {
		if dep, err := s.st.GetDeployment(id); err == nil {
			deps = append(deps, *dep)
		}
		rows, err := s.st.DeploymentEvents(id, snapshotEventsPerScope)
		if err != nil {
			return nil, err
		}
		for _, e := range rows {
			if inWindow(e) {
				events = append(events, e)
			}
		}
	}
	docs["deployments"] = deps
	docs["events"] = events

	images := map[string][]store.ImageNode{}
	for _, uuid := range scope.NodeUUIDs {
		node, err := s.st.NodeByUUID(uuid)
		if err != nil {
			continue
		}
		rows, err := s.st.ImageNodesForNode(node.ID)
		if err == nil {
			images[uuid] = rows
		}
	}
	docs["image_nodes"] = images
	if catalog, err := s.st.ListImages(); err == nil {
		docs["images"] = catalog
	}
	if storages, err := s.st.ListStorages(); err == nil {
		docs["storages"] = storages
	}
	if settings, err := s.st.AllSettings(); err == nil {
		docs["settings"] = settings
	}
	if s.ux != nil {
		requests := map[string][]uxrecord.Request{}
		for _, id := range depIDs {
			var rows []uxrecord.Request
			cursor := ""
			for len(rows) < snapshotRequestsPerScope {
				page, next, err := s.ux.Requests(ctx, uxstore.RequestQuery{DeploymentID: id, FromMS: scope.From.UnixMilli(), ToMS: scope.To.UnixMilli(), Limit: uxstore.MaxRequestLimit, Cursor: cursor})
				if err != nil {
					break
				}
				rows = append(rows, page...)
				if next == "" {
					break
				}
				cursor = next
			}
			requests[strconv.FormatUint(uint64(id), 10)] = rows
		}
		docs["requests"] = requests
	}
	return docs, nil
}

// ServerLogFiles are the log file and the rotated generations beside it,
// newest first.
func (s diagnosticsSources) ServerLogFiles() []string {
	if s.logFile == "" {
		return nil
	}
	out := []string{s.logFile}
	for i := 1; ; i++ {
		name := s.logFile + "." + strconv.Itoa(i)
		if _, err := os.Stat(name); err != nil {
			return out
		}
		out = append(out, name)
	}
}

func (s diagnosticsSources) Version() string { return version.String() }
