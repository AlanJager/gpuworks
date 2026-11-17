package app

import (
	"context"
	"crypto/hkdf"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"regexp"

	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/serverconfig"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Host is what a module's Init reaches the server through. It hands out the
// module's own tables, never the Store or a raw database handle, and collects
// what the module registers; registrations are accepted only while Init runs.
type Host struct {
	name    string
	log     *slog.Logger
	bus     *bus.Bus
	dataDir string
	install string
	master  string
	section serverconfig.File
	tables  *store.ExtensionTables
	st      *store.Store
	reg     *extensionSet
}

// ErrHostClosed is returned (or, for registrations without an error result,
// panicked with) when a module registers after its Init returned.
var ErrHostClosed = errors.New("extension host: registrations are accepted only while Init runs")

// Log is the server's logger, with module=<name> on every record.
func (h *Host) Log() *slog.Logger { return h.log }

// Bus is the server's event bus. A subscriber must not block the publisher.
func (h *Host) Bus() *bus.Bus { return h.bus }

// DataDir is the server's data directory. A module keeps its files under a
// directory named after itself.
func (h *Host) DataDir() string { return h.dataDir }

// InstallID names this installation; it is stable for the life of the data
// directory's master key and reveals nothing of it.
func (h *Host) InstallID() string { return h.install }

// Section decodes server.yaml's extensions.<name> into v, refusing keys v
// does not have. An absent section leaves v as it is.
func (h *Host) Section(v any) error { return h.section.Section(h.name, v) }

// Tables is the handle to the module's own tables (its Migrator's set); nil
// for a module without one. It is the only way a module reaches the
// database: core data goes through the API and the hooks below.
func (h *Host) Tables() *store.ExtensionTables { return h.tables }

var keyPurposePattern = regexp.MustCompile(`^[a-z][a-z0-9_.-]{0,63}$`)

// DeriveKey is a 32-byte secret for purpose, derived from the installation's
// master key with HKDF-SHA256 and bound to the module's name, so two modules,
// or two purposes of one module, never share a key and none reveals the
// master key. It is the same for the life of the master key. purpose follows
// [a-z][a-z0-9_.-]{0,63}; a module that needs a signing key seeds one with it
// (ed25519.NewKeyFromSeed).
func (h *Host) DeriveKey(purpose string) ([]byte, error) {
	if !keyPurposePattern.MatchString(purpose) {
		return nil, fmt.Errorf("extension %s: key purpose %q does not match [a-z][a-z0-9_.-]{0,63}", h.name, purpose)
	}
	return hkdf.Key(sha256.New, []byte(h.master), []byte("gpuworks extension key"), h.name+"\x00"+purpose, 32)
}

// MountAPI adds e's routes under /api/v1/x/<e.Name()>/ (api.Extension).
func (h *Host) MountAPI(e api.Extension) {
	h.mustBeOpen("MountAPI")
	if e == nil {
		panic(fmt.Sprintf("extension %s: MountAPI(nil)", h.name))
	}
	h.reg.apis = append(h.reg.apis, e)
}

// AddBannerSource adds notices above every console page (api.BannerSource).
func (h *Host) AddBannerSource(b api.BannerSource) {
	h.mustBeOpen("AddBannerSource")
	if b == nil {
		panic(fmt.Sprintf("extension %s: AddBannerSource(nil)", h.name))
	}
	h.reg.banners = append(h.reg.banners, b)
}

// DeclareFeatures declares feature keys the module's routes, menu entries
// and guards name. A key is declared once across all modules.
func (h *Host) DeclareFeatures(features ...featuregate.Feature) {
	h.mustBeOpen("DeclareFeatures")
	h.reg.features = append(h.reg.features, features...)
}

// ProvideFeatures makes src the answer for every declared feature. At most
// one module provides it; without one, every declared feature is locked
// (hidden when concealed).
func (h *Host) ProvideFeatures(src featuregate.Source) error {
	if h.reg.closed {
		return ErrHostClosed
	}
	if src == nil {
		return fmt.Errorf("extension %s: ProvideFeatures(nil)", h.name)
	}
	if h.reg.sourceBy != "" {
		return fmt.Errorf("extension %s: features are already provided by %s", h.name, h.reg.sourceBy)
	}
	h.reg.source, h.reg.sourceBy = src, h.name
	return nil
}

// GuardNodeInsert adds g to every new node's insert (store.NodeEnrollmentGuard):
// guards run in registration order inside the insert's writer transaction,
// and the first refusal wins. g must be pure: it gets the fleet's counts and
// must not call the Store, the module's Tables or anything that waits on
// them.
func (h *Host) GuardNodeInsert(g store.NodeEnrollmentGuard) {
	h.mustBeOpen("GuardNodeInsert")
	if g == nil {
		panic(fmt.Sprintf("extension %s: GuardNodeInsert(nil)", h.name))
	}
	h.st.AddNodeEnrollmentGuard(g)
}

// HoldScheduling registers a holder that keeps nodes out of scheduling
// (store.SchedulingHolder): it is asked on every status report inside the
// writer's transaction, so it must be pure and O(1). Holders are keyed by
// their source; a second holder with a source already registered is refused.
// The server applies every holder's answers once Init has run, and again
// whenever a module calls ReapplySchedulingHolds.
func (h *Host) HoldScheduling(holder store.SchedulingHolder) error {
	if h.reg.closed {
		return ErrHostClosed
	}
	if err := h.st.AddSchedulingHolder(holder); err != nil {
		return fmt.Errorf("extension %s: %w", h.name, err)
	}
	h.reg.holders++
	return nil
}

// ReapplySchedulingHolds asks the server to apply every holder's answers to
// the whole fleet (store.ApplySchedulingHolds) instead of waiting for each
// node's next status report, up to a minute away. A module calls it when its
// answers change. It never blocks and may be called from any goroutine, at
// any time, except from inside a hook (SchedulingHolder.Hold, a
// NodeEnrollmentGuard): a pass asks every holder again, so a holder that
// asked for one from Hold would keep the fleet in passes. Calls are
// coalesced: any number made while a pass is pending or running make at
// most one more pass, at least two seconds after the last one. The pass
// runs with the modules' services and stops with the server; a failed pass
// is logged and retried with a backoff. With no holder registered it does
// nothing.
func (h *Host) ReapplySchedulingHolds() { h.reg.holds.Request() }

// NodeFleet is every node's hold facts, ordered by id (enrollment order),
// read in one query on the read pool (store.NodeFleet). It is for a module
// that derives its answers from the whole fleet: at boot, and for a
// recompute after node lifecycle changes (store.NodeFleetTopic on the Bus,
// read with store.NodeFleetChangeOf), coalesced; never per status report or
// per event, and never from inside a hook. Subscribe before the first read,
// so no change between them is missed.
func (h *Host) NodeFleet(ctx context.Context) ([]store.NodeHoldFacts, error) {
	return h.st.NodeFleet(ctx)
}

func (h *Host) mustBeOpen(method string) {
	if h.reg.closed {
		panic(fmt.Sprintf("extension %s: %s: %v", h.name, method, ErrHostClosed))
	}
}
