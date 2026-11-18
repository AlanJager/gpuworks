package pluginbridge

import (
	"context"
	"strings"
	"time"
	"unicode/utf8"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/pushstate"
)

// MaxOverrideReason is the longest reason an override records, in bytes
// (the pstate_override column).
const MaxOverrideReason = 1024

// Administrator is who asks for an override, for the record and the
// audit line: the user id is what the override stores, the username what
// a reader of the log recognizes.
type Administrator struct {
	UserID   uint
	Username string
}

// OverrideRequest is one override asked for through the API.
type OverrideRequest struct {
	// Owner is the package whose restriction is loosened.
	Owner string
	// Kind is node_holds or fleet_capacity.
	Kind string
	// Entry is a node id in decimal for node_holds, max_nodes_with_cards
	// or max_cards for fleet_capacity.
	Entry string
	// Action is delete or loosen; Value is the raised cap of a loosen.
	Action string
	Value  *int64
	Reason string
	By     Administrator
}

// Override releases one restriction entry an owner pushed. The owner may be
// running, down, broken, revoked or uninstalled with its rows retained:
// what decides is that pushed state holds the entry. A new push of that
// entry by the owner supersedes the override. A missing or overlong reason
// and every rule of pushed state refuse with PLUGIN_OVERRIDE_REJECTED.
// Every call is audited, refused or not.
func (b *Bridge) Override(ctx context.Context, req OverrideRequest) (o pushstate.Override, err error) {
	b.admin.Lock()
	defer b.admin.Unlock()
	defer func() {
		b.auditOverride("override", req.Owner, req.Kind, req.Entry, o.ID, req.Reason, req.By, err)
	}()
	reason := strings.TrimSpace(req.Reason)
	if reason == "" || len(reason) > MaxOverrideReason || !utf8.ValidString(reason) {
		return pushstate.Override{}, messages.NewProductError(messages.ErrorPluginOverrideRejected, "an override needs a reason of at most 1024 bytes",
			map[string]any{"plugin": req.Owner, "kind": req.Kind, "entry": req.Entry, "rule": "reason"})
	}
	return b.state.Override(ctx, pushstate.OverrideRequest{Owner: req.Owner, Kind: req.Kind, Entry: req.Entry, Action: req.Action,
		Value: req.Value, Reason: reason, ByUser: req.By.UserID})
}

// RemoveOverride withdraws override id, so its owner's entry is enforced
// again as last pushed. An id not in force is PLUGIN_OVERRIDE_NOT_FOUND.
// Every call is audited, refused or not.
func (b *Bridge) RemoveOverride(ctx context.Context, id int64, by Administrator) (o pushstate.Override, err error) {
	b.admin.Lock()
	defer b.admin.Unlock()
	defer func() { b.auditOverride("remove_override", o.Owner, o.Kind, o.Entry, id, "", by, err) }()
	return b.state.RemoveOverride(ctx, id)
}

// Overrides are the overrides in force, by id.
func (b *Bridge) Overrides() []pushstate.Override { return b.state.Overrides() }

// auditOverride records an override call: who released or restored which
// entry of which package, and how it ended.
func (b *Bridge) auditOverride(action, owner, kind, entry string, id int64, reason string, by Administrator, err error) {
	outcome, code := "ok", ""
	if err != nil {
		outcome = "refused"
		code, _, _ = codeOf(err)
	}
	attrs := []any{"audit", true, "action", action, "plugin", owner, "kind", kind, "entry", entry, "override", id,
		"by_user", by.UserID, "by", by.Username, "outcome", outcome, "code", code}
	if reason != "" {
		attrs = append(attrs, "reason", reason)
	}
	b.log.Info("plugin override", attrs...)
}

// auditedEffects wraps pushed state's effects so a superseded override and
// a clock correction each leave an audit line before the server's own
// effect runs.
func (b *Bridge) auditedEffects(e pushstate.Effects) pushstate.Effects {
	superseded, corrected := e.OverrideSuperseded, e.ClockCorrected
	e.OverrideSuperseded = func(o pushstate.Override) {
		b.log.Info("plugin override", "audit", true, "action", "override_superseded", "plugin", o.Owner, "kind", o.Kind, "entry", o.Entry,
			"override", o.ID, "by_user", o.ByUser, "outcome", "ok", "code", messages.ErrorPluginOverrideSuperseded)
		if superseded != nil {
			superseded(o)
		}
	}
	e.ClockCorrected = func(from, to time.Time, by string) {
		b.log.Info("plugin clock corrected", "audit", true, "action", "clock_corrected", "from", from.Format(time.RFC3339),
			"to", to.Format(time.RFC3339), "by", by, "code", messages.ErrorPluginClockCorrected)
		if corrected != nil {
			corrected(from, to, by)
		}
	}
	return e
}
