package api

// A deployment's schedule, idle sleep and node replacement: orders given in
// advance that the scheduling automation (internal/server/automation) carries
// out as an operator's own orders.

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Bounds of the idle sleep wait, in seconds.
const (
	idleSleepMinSec         = 60
	idleSleepMaxSec         = 7 * 24 * 3600
	scheduleNextHorizonDays = 8
)

func (a *apiHandlers) listSchedule(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	rows, err := a.d.Store.LifecycleSchedules(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	items := make([]dto.LifecycleSchedule, 0, len(rows))
	for _, row := range rows {
		items = append(items, scheduleDTO(row, time.Now().UTC()))
	}
	writeJSON(w, http.StatusOK, dto.List[dto.LifecycleSchedule]{Items: items, Total: len(items)})
}

func (a *apiHandlers) createSchedule(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	row, ok := a.scheduleFromRequest(w, r, dep)
	if !ok {
		return
	}
	if err := a.d.Store.CreateLifecycleSchedule(row); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusCreated, scheduleDTO(*row, time.Now().UTC()))
}

func (a *apiHandlers) updateSchedule(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	id := parseUint(chi.URLParam(r, "sid"))
	if _, err := a.d.Store.LifecycleSchedule(dep.ID, id); err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	row, ok := a.scheduleFromRequest(w, r, dep)
	if !ok {
		return
	}
	row.ID = id
	if err := a.d.Store.UpdateLifecycleSchedule(row); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, scheduleDTO(*row, time.Now().UTC()))
}

func (a *apiHandlers) deleteSchedule(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	if err := a.d.Store.DeleteLifecycleSchedule(dep.ID, parseUint(chi.URLParam(r, "sid"))); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, err)
			return
		}
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// scheduleFromRequest reads and validates one schedule order for dep,
// writing the response when it is refused.
func (a *apiHandlers) scheduleFromRequest(w http.ResponseWriter, r *http.Request, dep *store.Deployment) (*store.LifecycleSchedule, bool) {
	var req dto.LifecycleScheduleRequest
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return nil, false
	}
	invalid := func(field, reason string) (*store.LifecycleSchedule, bool) {
		writeProductError(w, http.StatusBadRequest, messages.ErrorScheduleInvalid, fmt.Sprintf("schedule %s: %s", field, reason), map[string]any{"field": field, "reason": reason})
		return nil, false
	}
	row := &store.LifecycleSchedule{DeploymentID: dep.ID, Action: req.Action, Timezone: strings.TrimSpace(req.Timezone), Enabled: req.Enabled == nil || *req.Enabled}
	if req.Role != "" && req.Action != store.ScheduleActionReplicas {
		return invalid("role", "only a replicas order names a role")
	}
	if len(req.Weekdays) == 0 {
		return invalid("weekdays", "name at least one day")
	}
	for _, day := range req.Weekdays {
		if day < 0 || day > 6 {
			return invalid("weekdays", "days are 0 (Sunday) to 6 (Saturday)")
		}
		if row.Weekdays&(1<<day) != 0 {
			return invalid("weekdays", "a day is named twice")
		}
		row.Weekdays |= 1 << day
	}
	minute, ok := parseScheduleTime(req.Time)
	if !ok {
		return invalid("time", "use HH:MM, 00:00 to 23:59")
	}
	row.Minute = minute
	if row.Timezone != "" {
		if _, err := time.LoadLocation(row.Timezone); err != nil {
			writeProductError(w, http.StatusBadRequest, messages.ErrorScheduleTimezoneUnknown, "the server does not know this time zone", map[string]any{"timezone": row.Timezone})
			return nil, false
		}
	}
	switch req.Action {
	case store.ScheduleActionSleep, store.ScheduleActionWake:
		if req.Replicas != nil {
			return invalid("replicas", "only a replicas order sets a replica count")
		}
		if code := planning.SleepEligibility(dep); code != "" {
			writeSleepRefusal(w, http.StatusBadRequest, &planning.SleepRefusal{Code: code, Params: map[string]any{"engine": dep.Engine}})
			return nil, false
		}
		if req.Action == store.ScheduleActionWake {
			if req.SleepLevel != nil {
				return invalid("sleep_level", "only a sleep order sets a level")
			}
			break
		}
		if req.SleepLevel == nil || (*req.SleepLevel != proto.SleepLevelL1 && *req.SleepLevel != proto.SleepLevelL2) {
			return invalid("sleep_level", "a sleep order sleeps at level 1 or 2")
		}
		row.SleepLevel = *req.SleepLevel
	case store.ScheduleActionReplicas:
		if req.SleepLevel != nil {
			return invalid("sleep_level", "only a sleep order sets a level")
		}
		role := strings.TrimSpace(req.Role)
		if err := scheduledReplicasRefusal(dep, role); err != nil {
			writeErr(w, http.StatusBadRequest, err)
			return nil, false
		}
		lowest, highest := scheduledReplicasBounds(dep, role)
		if req.Replicas == nil || *req.Replicas < lowest || *req.Replicas > highest {
			return invalid("replicas", fmt.Sprintf("a replica count is %d to %d", lowest, highest))
		}
		row.Replicas = *req.Replicas
		row.Role = role
	default:
		return invalid("action", "an order is sleep, wake or replicas")
	}
	return row, true
}

// parseScheduleTime reads HH:MM into minutes after midnight.
func parseScheduleTime(raw string) (int, bool) {
	hh, mm, ok := strings.Cut(strings.TrimSpace(raw), ":")
	if !ok || len(hh) != 2 || len(mm) != 2 {
		return 0, false
	}
	h, errH := strconv.Atoi(hh)
	m, errM := strconv.Atoi(mm)
	if errH != nil || errM != nil || h < 0 || h > 23 || m < 0 || m > 59 {
		return 0, false
	}
	return h*60 + m, true
}

// automationSchedule is a stored order as the automation reads it. An unknown
// zone (validated on write, so only a zone the server lost since) falls back
// to the server's own.
func automationSchedule(row store.LifecycleSchedule) automation.Schedule {
	loc := time.Local
	if row.Timezone != "" {
		if l, err := time.LoadLocation(row.Timezone); err == nil {
			loc = l
		}
	}
	s := automation.Schedule{ID: row.ID, DeploymentID: row.DeploymentID, Action: row.Action, Weekdays: row.Weekdays, Minute: row.Minute, Location: loc, SleepLevel: row.SleepLevel, Replicas: row.Replicas, Role: row.Role}
	if row.LastFiredAt != nil {
		s.LastFiredAt = *row.LastFiredAt
	}
	return s
}

func scheduleDTO(row store.LifecycleSchedule, now time.Time) dto.LifecycleSchedule {
	out := dto.LifecycleSchedule{
		ID: row.ID, Action: row.Action, Weekdays: []int{}, Time: fmt.Sprintf("%02d:%02d", row.Minute/60, row.Minute%60),
		Timezone: row.Timezone, SleepLevel: row.SleepLevel, Enabled: row.Enabled,
	}
	if row.Action == store.ScheduleActionReplicas {
		// A PD role may be scheduled down to 0, so the count is stated even then.
		out.Replicas, out.Role = &row.Replicas, row.Role
	}
	for day := 0; day < 7; day++ {
		if row.Weekdays&(1<<day) != 0 {
			out.Weekdays = append(out.Weekdays, day)
		}
	}
	if row.Enabled {
		if next := automationSchedule(row).Occurrences(now, now.AddDate(0, 0, scheduleNextHorizonDays)); len(next) > 0 {
			at := next[0].UTC()
			out.NextAt = &at
		}
	}
	return out
}

func (a *apiHandlers) setIdleSleep(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	var req dto.IdleSleep
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, err)
		return
	}
	if req.AfterSec != 0 && (req.AfterSec < idleSleepMinSec || req.AfterSec > idleSleepMaxSec) {
		writeProductError(w, http.StatusBadRequest, messages.ErrorIdleSleepInvalid, fmt.Sprintf("after_sec is 0 (off) or %d to %d", idleSleepMinSec, idleSleepMaxSec), map[string]any{"field": "after_sec", "minimum": idleSleepMinSec, "maximum": idleSleepMaxSec})
		return
	}
	if req.AfterSec == 0 && req.Level == 0 {
		// Turning idle sleep off need not restate a level: the one set
		// before is kept for when it is turned on again.
		req.Level = max(dep.IdleSleepLevel, proto.SleepLevelL1)
	}
	if req.Level != proto.SleepLevelL1 && req.Level != proto.SleepLevelL2 {
		writeProductError(w, http.StatusBadRequest, messages.ErrorIdleSleepInvalid, "level is 1 or 2", map[string]any{"field": "level"})
		return
	}
	if req.AfterSec > 0 {
		if code := planning.SleepEligibility(dep); code != "" {
			writeSleepRefusal(w, http.StatusBadRequest, &planning.SleepRefusal{Code: code, Params: map[string]any{"engine": dep.Engine}})
			return
		}
	}
	if err := a.d.Store.SetDeploymentIdleSleep(dep.ID, req.AfterSec, req.Level); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	writeJSON(w, http.StatusOK, req)
}

// endWakeWaiting withdraws every wake of the deployment that waits for room.
func (a *apiHandlers) endWakeWaiting(w http.ResponseWriter, r *http.Request) {
	dep, err := a.d.Store.GetDeployment(parseUint(chi.URLParam(r, "id")))
	if err != nil {
		writeErr(w, http.StatusNotFound, err)
		return
	}
	defer planning.LockLifecycle()()
	insts, err := a.d.Store.InstancesForDeployment(dep.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	var waiting []string
	for _, in := range insts {
		if in.WakeWaitingSince != nil {
			waiting = append(waiting, in.UUID)
		}
	}
	if err := a.d.Store.EndWakeWaiting(dep.ID, waiting); err != nil {
		writeErr(w, http.StatusInternalServerError, err)
		return
	}
	slices.Sort(waiting)
	if waiting == nil {
		waiting = []string{}
	}
	writeJSON(w, http.StatusOK, dto.List[string]{Items: waiting, Total: len(waiting)})
}
