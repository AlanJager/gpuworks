package pushstate

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/fleetcap"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store/pstate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

const opsHolds = `{"nodes":{"12":{"code":"ACME_MAINTENANCE","params":{"ticket":"OPS-118"}},"9":{"code":"ACME_QUARANTINE"}}}`

const authorityBanners = `{"banners":[{"audience":"admin","id":"grace","steps":[{"notice":{"code":"ACME_GRACE","params":{"days":30}},"severity":"warning"}]},{"audience":"all","id":"usb","steps":[{"notice":{"code":"ACME_GRACE"},"severity":"danger"}]}]}`

// The core is killed with plugins' state committed and boots again with no
// plugin running: everything pushed is enforced before any plugin starts.
func TestStateIsEnforcedAfterACoreRestartBeforeAnyPluginRuns(t *testing.T) {
	dsn := durableDSN(t)
	clk := newFakeClock("2026-12-01T00:00:00Z")
	{
		st := openStore(t, dsn)
		h := newHarness(t, st, clk).load().run()
		authority := pusherOf(acmeAuthority, "push:banners")
		h.push(authority, extapi.KindAvailability, 1, backupFull)
		h.push(authority, extapi.KindFleetCapacity, 1, capacityBody(2, 16))
		h.push(authority, extapi.KindBanners, 1, authorityBanners)
		h.push(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, opsHolds)
		h.push(authority, extapi.KindAttestations, 1, `{"attestations":[{"blob":"c3RhdGVtZW50","feature":"acme.backup","format":"acme.grant","name":"grant"}]}`)
		// Killed: the store goes away under it, so not even its shutdown
		// commit lands.
		_ = st.Close()
		h.stop()
	}

	st := openStore(t, dsn)
	clk.advance(time.Minute)
	h := newHarness(t, st, clk)
	h.setFleet(fleetcap.Node{ID: 1, Cards: 8}, fleetcap.Node{ID: 2, Cards: 8}, fleetcap.Node{ID: 3, Cards: 8})
	h.load()

	if a := h.s.Availability("acme.backup"); a.Access != featuregate.Full || len(a.Limits) != 1 {
		t.Fatalf("availability after restart: %+v", a)
	}
	if refusal := h.s.GuardNode(2, 16); refusal == nil || refusal.Code() != "ACME_NODE_LIMIT" || refusal.Params()["max"] != int64(2) {
		t.Fatalf("guard after restart: %v", refusal)
	}
	if h.s.GuardNode(1, 8) != nil {
		t.Fatal("guard refused a fleet with room")
	}
	if hold := h.s.Hold(12, 8); hold.Code != "ACME_MAINTENANCE" || hold.Params["ticket"] != "OPS-118" {
		t.Fatalf("explicit hold after restart: %+v", hold)
	}
	if hold := h.s.Hold(3, 8); hold.Code != "ACME_OVER_CAPACITY" {
		t.Fatalf("over-capacity node after restart: %+v", hold)
	}
	if h.s.Hold(1, 8).held() || h.s.Hold(2, 8).held() {
		t.Fatal("a node within capacity is held")
	}
	if all, admin := h.s.Banners(false), h.s.Banners(true); len(all) != 1 || len(admin) != 2 || admin[0].Severity != extapi.SeverityDanger {
		t.Fatalf("banners after restart: all=%+v admin=%+v", all, admin)
	}
	if att := h.s.Attestations("acme"); len(att) != 1 || string(att[0].Blob) != "statement" || att[0].Owner != "acme_authority" {
		t.Fatalf("attestations after restart: %+v", att)
	}
	if len(h.s.Attestations("rogue")) != 0 {
		t.Fatal("attestations relayed to another publisher")
	}
	held := h.s.Held("acme_authority")
	if len(held) != 4 {
		t.Fatalf("held after restart = %+v", held)
	}
	// Owners holding state were recorded down from this boot.
	snap, err := st.LoadPushedState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Down) != 2 || len(snap.Markers) != len(Markers()) {
		t.Fatalf("down=%+v markers=%+v", snap.Down, snap.Markers)
	}
}

func TestLoadFailsClosed(t *testing.T) {
	ctx := context.Background()
	t.Run("the store cannot be read", func(t *testing.T) {
		st := openStore(t, "")
		st.failLoad.Store(true)
		if err := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).s.Load(ctx); !errors.Is(err, errInjected) {
			t.Fatalf("load = %v", err)
		}
	})
	t.Run("a newer core wrote the state", func(t *testing.T) {
		st := openStore(t, "")
		if _, err := st.Store.WritePushedState(ctx, &pstate.Batch{Markers: []pstate.Marker{{Name: "pstate.node_holds", Version: 2, WrittenBy: "v27.3.0"}}}); err != nil {
			t.Fatal(err)
		}
		err := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).s.Load(ctx)
		if extapi.CodeOf(err) != extapi.ErrorStateTooNew {
			t.Fatalf("load = %v", err)
		}
		var e *extapi.Error
		if !errors.As(err, &e) || e.Params["needs"] != "v27.3.0" || e.Params["marker"] != "pstate.node_holds" {
			t.Fatalf("too new = %+v", e)
		}
	})
	t.Run("a stored restriction is corrupt", func(t *testing.T) {
		st := openStore(t, "")
		w := pstate.TableWrite{Record: pstate.Owner{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Schema: 1, Generation: 1, Digest: "sha256:x"}}
		w.Insert.NodeHolds = []pstate.NodeHold{{Owner: "acme_ops", NodeID: 4, Code: "not a code"}}
		if _, err := st.Store.WritePushedState(ctx, &pstate.Batch{Tables: []pstate.TableWrite{w}}); err != nil {
			t.Fatal(err)
		}
		if err := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).s.Load(ctx); !errors.Is(err, ErrCorruptRestriction) {
			t.Fatalf("load = %v", err)
		}
	})
	t.Run("a corrupt grant is left out", func(t *testing.T) {
		st := openStore(t, "")
		w := pstate.TableWrite{Record: pstate.Owner{Owner: "acme_authority", Kind: extapi.KindAvailability, Schema: 1, Generation: 1, Digest: "sha256:x"}}
		w.Insert.AvailabilityRows = []pstate.AvailabilityRow{{Owner: "acme_authority", Feature: "acme.backup", DownAccess: "full"}}
		if _, err := st.Store.WritePushedState(ctx, &pstate.Batch{Tables: []pstate.TableWrite{w}}); err != nil {
			t.Fatal(err)
		}
		h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load()
		if a := h.s.Availability("acme.backup"); a.Access != featuregate.Locked || a.Notice.Code() != messages.ErrorFeatureProviderAbsent {
			t.Fatalf("corrupt grant = %+v", a)
		}
	})
	t.Run("recording owners down fails", func(t *testing.T) {
		st := openStore(t, "")
		h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
		h.push(pusherOf(acmeAuthority), extapi.KindAvailability, 1, backupFull)
		h.stop()
		st.failWrites.Store(1)
		if err := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).s.Load(ctx); !errors.Is(err, errInjected) {
			t.Fatalf("load = %v", err)
		}
	})
}

// Every golden table of schema 1 survives the typed rows: normalized,
// written, read back and rebuilt, it equals what was pushed.
func TestEveryGoldenTableRoundTripsThroughTheTypedRows(t *testing.T) {
	goldens := map[string][]string{
		extapi.KindAvailability:  {backupFull, `{"down_grace_s":3600,"rows":[{"down_access":"read_only","feature":"acme.backup","steps":[{"access":"full","limits":[{"dim":"concurrent_jobs","max":4},{"dim":"gpus","unlimited":true}]},{"access":"full","at":"2027-01-01T00:00:00Z","link":"/x/acme_authority/renew","notice":{"code":"ACME_TERM_GRACE","params":{"days":30}}},{"access":"read_only","at":"2027-02-01T00:00:00Z","link":"https://acme.example/renew","notice":{"code":"FEATURE_EXPIRED"}}]},{"display":{"plan":"pro"},"down_access":"locked","feature":"acme.reports","steps":[{"access":"locked","notice":{"code":"ACME_NOT_PURCHASED","params":{"flag":true,"who":"x"}}}]}]}`, `{}`},
		extapi.KindFleetCapacity: {capacityBody(8, 64), keepBody(8, 64, "3,7,12"), keepBody(8, 64, "7,9"), `{"hold":{"code":"ACME_OVER_CAPACITY"},"refuse_card":{"code":"ACME_CARD_LIMIT"},"refuse_node":{"code":"ACME_NODE_LIMIT"},"select":"enrollment_order","steps":[{},{"at":"2027-02-01T00:00:00Z","max_cards":16}]}`},
		extapi.KindNodeHolds:     {opsHolds, `{}`},
		extapi.KindBanners:       {authorityBanners, `{"banners":[{"audience":"admin","display":{"icon":"key"},"id":"grace","steps":[{"link":"/x/acme_authority","notice":{"code":"ACME_TERM_GRACE"},"severity":"warning"},{"at":"2027-02-01T00:00:00Z","hidden":true}]}]}`},
		extapi.KindAttestations:  {`{"attestations":[{"blob":"c3RhdGVtZW50LTE=","feature":"acme.backup","format":"acme.grant-statement.v1","name":"grant_backup"},{"blob":"AAEC","format":"acme.binding.v1","name":"install_binding"}]}`},
	}
	st := openStore(t, "")
	ctx := context.Background()
	gen := int64(0)
	for kind, bodies := range goldens {
		var prev *part
		for _, body := range bodies {
			table, err := extapi.DecodeTable(kind, 1, []byte(body))
			if err != nil {
				t.Fatalf("%s %s: %v", kind, body, err)
			}
			gen++
			next := &part{owner: "acme_authority", kind: kind, schema: 1, gen: uint64(gen), digest: "sha256:x", table: normalize(table)}
			w := tableWrite(prev, next)
			if _, err := st.WritePushedState(ctx, &pstate.Batch{Tables: []pstate.TableWrite{w}}); err != nil {
				t.Fatal(err)
			}
			snap, err := st.LoadPushedState(ctx)
			if err != nil {
				t.Fatal(err)
			}
			tables, err := tablesOf(&snap)
			if err != nil {
				t.Fatal(err)
			}
			got := tables[ownerKind{"acme_authority", kind}]
			if !reflect.DeepEqual(entriesOf(got), entriesOf(next.table)) {
				t.Fatalf("%s round trip:\n got %#v\nwant %#v", kind, got, next.table)
			}
			if err := got.Validate(); err != nil {
				t.Fatalf("%s read back does not validate: %v", kind, err)
			}
			prev = next
		}
	}
}
