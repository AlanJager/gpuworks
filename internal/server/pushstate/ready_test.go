package pushstate

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

const graceTen = `{"down_grace_s":600,"rows":[{"down_access":"read_only","feature":"acme.backup","steps":[{"access":"full"}]}]}`

func (h *harness) access() featuregate.Access { return h.s.Availability("acme.backup").Access }

// A process that declared itself ready gets no boot extension when it goes
// down: its grants clamp when the grace ends. A later process that has not
// declared itself ready gets the extension again, and a replaced process
// cannot end it.
func TestReadyEndsTheBootExtensionOfTheProcessThatDeclaredIt(t *testing.T) {
	clk := newFakeClock("2026-12-01T00:00:00Z")
	h := newHarness(t, openStore(t, ""), clk).load().run()
	h.push(pusherOf(acmeAuthority), extapi.KindAvailability, 1, graceTen)
	h.s.OwnerUp("acme_authority", 1)
	if !h.s.Ready("acme_authority", 1) {
		t.Fatal("the first ready did not end the extension")
	}
	if h.s.Ready("acme_authority", 1) {
		t.Fatal("a second ready reported a change")
	}
	h.s.OwnerDown("acme_authority")
	clk.advance(10*time.Minute - time.Second)
	h.recombine()
	if h.access() != featuregate.Full {
		t.Fatal("clamped before the grace")
	}
	clk.advance(time.Second)
	h.recombine()
	if h.access() != featuregate.ReadOnly {
		t.Fatalf("the boot extension still applied after ready: %v", h.access())
	}

	// The next process has not declared itself ready: its down period has
	// the extension again.
	h.s.OwnerUp("acme_authority", 2)
	if h.access() != featuregate.Full {
		t.Fatal("not as pushed with the owner back")
	}
	if h.s.Ready("acme_authority", 1) {
		t.Fatal("a replaced process ended the extension of its successor")
	}
	h.s.OwnerDown("acme_authority")
	clk.advance(10*time.Minute + time.Second)
	h.recombine()
	if h.access() != featuregate.Full {
		t.Fatal("a process that never declared ready lost its boot extension")
	}
	clk.advance(BootExtension)
	h.recombine()
	if h.access() != featuregate.ReadOnly {
		t.Fatalf("after grace and extension: %v", h.access())
	}
}

// A ready that arrives while its process is already counted down, during
// the extension, ends the extension at the call rather than at +2 minutes.
func TestReadyDuringTheExtensionEndsItAtTheCall(t *testing.T) {
	clk := newFakeClock("2026-12-01T00:00:00Z")
	h := newHarness(t, openStore(t, ""), clk).load().run()
	h.push(pusherOf(acmeAuthority), extapi.KindAvailability, 1, graceTen)
	h.s.OwnerUp("acme_authority", 1)
	h.s.OwnerDown("acme_authority")
	clk.advance(10*time.Minute + 30*time.Second)
	h.recombine()
	if h.access() != featuregate.Full {
		t.Fatal("clamped inside the boot extension")
	}
	if !h.s.Ready("acme_authority", 1) {
		t.Fatal("ready did not end the extension")
	}
	if h.access() != featuregate.ReadOnly {
		t.Fatalf("the extension outlived the ready: %v", h.access())
	}
}

// Ready after a crash and a restart of the core never renews the grace:
// the persisted down start stays, and the clamp follows it (A2).
func TestReadyAfterARestartDoesNotRenewTheGrace(t *testing.T) {
	dsn := durableDSN(t)
	clk := newFakeClock("2026-12-01T00:00:00Z")
	st := openStore(t, dsn)
	h := newHarness(t, st, clk).load().run()
	h.push(pusherOf(acmeAuthority), extapi.KindAvailability, 1, graceTen)
	h.s.OwnerUp("acme_authority", 1)
	h.s.OwnerDown("acme_authority")
	since := clk.clock().Wall().UnixMilli()
	h.stop()
	_ = st.Close()

	clk.advance(5 * time.Minute)
	st = openStore(t, dsn)
	h = newHarness(t, st, clk).load().run()
	if !h.s.Ready("acme_authority", 1) {
		t.Fatal("ready after the restart changed nothing")
	}
	snap, err := st.LoadPushedState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(snap.Down) != 1 || snap.Down[0].SinceMS != since {
		t.Fatalf("down after ready = %+v, want since %d", snap.Down, since)
	}
	clk.advance(5*time.Minute + time.Second)
	h.recombine()
	if h.access() != featuregate.ReadOnly {
		t.Fatalf("the grace was renewed: %v", h.access())
	}
}

// OwnerRows is the owner's own committed table: another owner's table of
// the same kind is invisible, a push waiting for its commit is not held,
// and after a restart the table answers from the stored rows with the
// digest the push was acknowledged with.
func TestOwnerRowsAreTheOwnersOwnCommittedTable(t *testing.T) {
	dsn := durableDSN(t)
	clk := newFakeClock("2026-12-01T00:00:00Z")
	st := openStore(t, dsn)
	h := newHarness(t, st, clk).load().run()
	ack := h.push(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 3, opsHolds)
	h.push(pusherOf(acmeAuthority, "push:node_holds"), extapi.KindNodeHolds, 1, `{"nodes":{"4":{"code":"ACME_QUARANTINE"}}}`)
	withDisplay := `{"banners":[{"audience":"admin","display":{"icon":"key"},"id":"grace","steps":[{"notice":{"code":"ACME_GRACE"},"severity":"warning"}]}]}`
	bannersAck := h.push(pusherOf(acmeOps, "push:banners"), extapi.KindBanners, 1, withDisplay)

	got, err := h.s.OwnerRows("acme_ops", extapi.KindNodeHolds)
	if err != nil {
		t.Fatal(err)
	}
	if err := got.Validate(); err != nil {
		t.Fatalf("answer %+v: %v", got, err)
	}
	if got.Generation != 3 || got.Schema != 1 || got.Digest != ack.Digest || string(got.Body) != opsHolds ||
		!got.PushedAt.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)) {
		t.Fatalf("own rows = %+v (%s)", got, got.Body)
	}
	if other, _ := h.s.OwnerRows("acme_backup", extapi.KindNodeHolds); other.Held() || other.Kind != extapi.KindNodeHolds {
		t.Fatalf("a package that pushed nothing sees %+v", other)
	}
	if none, _ := h.s.OwnerRows("acme_ops", extapi.KindAvailability); none.Held() {
		t.Fatalf("a kind the owner never pushed = %+v", none)
	}
	h.stop()
	_ = st.Close()

	st = openStore(t, dsn)
	h = newHarness(t, st, clk).load()
	again, err := h.s.OwnerRows("acme_ops", extapi.KindNodeHolds)
	if err != nil || again.Digest != ack.Digest || string(again.Body) != opsHolds {
		t.Fatalf("after restart = %+v (%s), %v", again, again.Body, err)
	}
	// The display contents are not stored: the body is what the core
	// holds, under the digest of what was pushed.
	b, err := h.s.OwnerRows("acme_ops", extapi.KindBanners)
	if err != nil || b.Validate() != nil || b.Digest != bannersAck.Digest {
		t.Fatalf("banners after restart = %+v, %v", b, err)
	}
	if _, err := b.Table(); err != nil {
		t.Fatalf("the held banners do not decode: %v", err)
	}
}

// Clocks reads the wall clock, extclock and the high-water mark as stored.
func TestClocksReportTheStoredHighWaterMark(t *testing.T) {
	clk := newFakeClock("2027-03-01T00:00:00Z")
	h := newHarness(t, openStore(t, ""), clk).load().run()
	if r := h.s.Clocks(); !r.HWM.IsZero() || !r.Wall.Equal(time.Date(2027, 3, 1, 0, 0, 0, 0, time.UTC)) || r.ExtClock.Before(r.Wall) {
		t.Fatalf("fresh clocks = %+v", r)
	}
	clk.setWall("2027-01-15T00:00:00Z")
	to := time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC)
	if err := h.s.CorrectClock(context.Background(), to, "acme_authority"); err != nil {
		t.Fatal(err)
	}
	r := h.s.Clocks()
	if !r.HWM.Equal(to) || !r.ExtClock.Equal(to) || !r.Wall.Equal(to) {
		t.Fatalf("clocks after a correction = %+v", r)
	}
}
