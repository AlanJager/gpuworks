package pushstate

import (
	"context"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The expiry step fires at its instant with its owner dead.
func TestAStepFiresAtItsInstantWithTheOwnerDown(t *testing.T) {
	clk := newFakeClock("2027-01-31T23:59:00Z")
	h := newHarness(t, openStore(t, ""), clk).load().run()
	h.push(pusherOf(acmeQuota), extapi.KindAvailability, 1, backupFull)
	h.s.OwnerDown("acme_quota")
	h.s.mu.Lock()
	next := h.s.next
	h.s.mu.Unlock()
	if want := time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC).UnixMilli(); next != want {
		t.Fatalf("timer armed for %d, want the step at %d", next, want)
	}
	if h.s.Availability("acme.backup").Access != featuregate.Full {
		t.Fatal("expired early")
	}
	clk.advance(time.Minute)
	h.recombine()
	a := h.s.Availability("acme.backup")
	if a.Access != featuregate.ReadOnly || a.Notice.Code() != "ACME_EXPIRED" {
		t.Fatalf("after the step: %+v", a)
	}
}

// The real timer, on the real clock, fires the step without a push or a
// poll in between.
func TestTheStepTimerFiresOnTheRealClock(t *testing.T) {
	at := time.Now().UTC().Add(1500 * time.Millisecond).Truncate(time.Second).Add(time.Second)
	body := `{"rows":[{"down_access":"locked","feature":"acme.backup","steps":[{"access":"full"},{"access":"locked","at":"` + at.Format(time.RFC3339) + `"}]}]}`
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-01-01T00:00:00Z"), func(c *Config) { c.Clock = Clock{} }).load().run()
	h.push(pusherOf(acmeQuota), extapi.KindAvailability, 1, body)
	h.s.OwnerDown("acme_quota")
	h.waitFor("the step to fire", func() bool { return h.s.Availability("acme.backup").Access == featuregate.Locked })
	if time.Now().Before(at) {
		t.Fatal("the step fired before its instant")
	}
}

func TestTurningTheWallClockBackCannotReviveAnExpiredStep(t *testing.T) {
	dsn := durableDSN(t)
	clk := newFakeClock("2027-02-01T00:00:30Z")
	st := openStore(t, dsn)
	h := newHarness(t, st, clk).load().run()
	h.push(pusherOf(acmeQuota), extapi.KindAvailability, 1, backupFull)
	if h.s.Availability("acme.backup").Access != featuregate.ReadOnly {
		t.Fatal("the step has not fired")
	}
	clk.setWall("2026-06-01T00:00:00Z")
	h.recombine()
	if h.s.Availability("acme.backup").Access != featuregate.ReadOnly {
		t.Fatal("turning the wall clock back revived the step")
	}
	// Across a restart too: the high-water mark is persisted at shutdown.
	h.stop()
	_ = st.Close()
	clk2 := newFakeClock("2026-06-01T00:00:00Z")
	h2 := newHarness(t, openStore(t, dsn), clk2).load()
	if a := h2.s.Availability("acme.backup"); a.Access != featuregate.ReadOnly {
		t.Fatalf("a restart with the wall clock back revived the step: %+v", a)
	}
	if now := h2.s.Now(); now.Before(time.Date(2027, 2, 1, 0, 0, 30, 0, time.UTC)) {
		t.Fatalf("extclock after restart = %v", now)
	}
}

func TestTheHighWaterMarkIsPersistedEveryMinute(t *testing.T) {
	st := openStore(t, "")
	clk := newFakeClock("2026-12-01T00:00:00Z")
	h := newHarness(t, st, clk).load()
	clk.advance(90 * time.Second)
	h.s.mu.Lock()
	h.s.hwmDirty = true // what the minute ticker sets
	h.s.mu.Unlock()
	h.s.commit(context.Background())
	snap, err := st.LoadPushedState(context.Background())
	if err != nil || snap.Clock == nil || snap.Clock.HWMMS != time.Date(2026, 12, 1, 0, 1, 30, 0, time.UTC).UnixMilli() {
		t.Fatalf("clock = %+v, %v", snap.Clock, err)
	}
	// A failed write keeps the mark owed.
	st.failWrites.Store(1)
	h.s.mu.Lock()
	h.s.hwmDirty = true
	h.s.mu.Unlock()
	h.s.commit(context.Background())
	h.s.mu.Lock()
	owed := h.s.hwmDirty
	h.s.mu.Unlock()
	if !owed {
		t.Fatal("a failed write dropped the high-water mark")
	}
}

func TestACorrectionUnfiresStepsAndIsBounded(t *testing.T) {
	st := openStore(t, "")
	// The wall clock ran ahead and the step fired; then the wall clock was
	// put right, but the high-water mark keeps extclock ahead.
	clk := newFakeClock("2027-03-01T00:00:00Z")
	var corrections []string
	h := newHarness(t, st, clk, func(c *Config) {
		c.Effects.ClockCorrected = func(from, to time.Time, by string) {
			corrections = append(corrections, from.Format(time.RFC3339)+">"+to.Format(time.RFC3339)+" by "+by)
		}
	}).load().run()
	h.push(pusherOf(acmeQuota), extapi.KindAvailability, 1, backupFull)
	clk.setWall("2027-01-15T00:00:00Z")
	h.recombine()
	if h.s.Availability("acme.backup").Access != featuregate.ReadOnly {
		t.Fatal("the step should still be fired")
	}
	tooFar := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	if err := h.s.CorrectClock(context.Background(), tooFar, "root"); err == nil {
		t.Fatal("a correction beyond the bound was applied")
	} else if pe, ok := err.(*messages.ProductError); !ok || pe.Code() != messages.ErrorPluginClockCorrectionTooLarge {
		t.Fatalf("too far = %v", err)
	}
	if err := h.s.CorrectClock(context.Background(), time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC), "acme_quota"); err != nil {
		t.Fatal(err)
	}
	if a := h.s.Availability("acme.backup"); a.Access != featuregate.Full {
		t.Fatalf("the correction did not un-fire the step: %+v", a)
	}
	if len(corrections) != 1 || corrections[0] != "2027-03-01T00:00:00Z>2027-01-15T00:00:00Z by acme_quota" {
		t.Fatalf("corrections = %v", corrections)
	}
	admin := h.s.Banners(true)
	if len(admin) != 1 || admin[0].Notice.Code() != messages.ErrorPluginClockCorrected {
		t.Fatalf("banners = %+v", admin)
	}
	snap, _ := st.LoadPushedState(context.Background())
	if snap.Clock == nil || snap.Clock.HWMMS != time.Date(2027, 1, 15, 0, 0, 0, 0, time.UTC).UnixMilli() {
		t.Fatalf("persisted clock = %+v", snap.Clock)
	}
	// The banner goes after a day.
	clk.advance(25 * time.Hour)
	h.recombine()
	if len(h.s.Banners(true)) != 0 {
		t.Fatal("the correction banner stayed")
	}
}

func TestGrantsClampAfterTheGraceAndRestrictionsNeverLapse(t *testing.T) {
	clk := newFakeClock("2026-12-01T00:00:00Z")
	h := newHarness(t, openStore(t, ""), clk).load().run()
	quota := pusherOf(acmeQuota)
	h.push(quota, extapi.KindAvailability, 1, `{"down_grace_s":600,"rows":[{"down_access":"read_only","feature":"acme.backup","steps":[{"access":"full"}]},{"down_access":"locked","feature":"acme.reports","steps":[{"access":"read_only","notice":{"code":"ACME_GRACE"}}]}]}`)
	h.push(quota, extapi.KindFleetCapacity, 1, capacityBody(1, 8))
	h.s.OwnerDown("acme_quota")

	clk.advance(10*time.Minute + BootExtension - time.Second)
	h.recombine()
	if h.s.Availability("acme.backup").Access != featuregate.Full {
		t.Fatal("clamped before the grace")
	}
	if b := h.s.Banners(true); len(b) != 1 || b[0].Notice.Code() != messages.ErrorPluginDown || b[0].Notice.Params()["plugin"] != "acme_quota" {
		t.Fatalf("down banner = %+v", b)
	}
	clk.advance(time.Second)
	h.recombine()
	a := h.s.Availability("acme.backup")
	if a.Access != featuregate.ReadOnly || a.Notice.Code() != messages.ErrorPluginUnavailable || a.Notice.Params()["plugin"] != "acme_quota" {
		t.Fatalf("after the grace: %+v", a)
	}
	if r := h.s.Availability("acme.reports"); r.Access != featuregate.Locked {
		t.Fatalf("down_access locked: %+v", r)
	}
	// Restrictions hold however long the owner stays down.
	clk.advance(30 * 24 * time.Hour)
	h.recombine()
	if h.s.GuardNode(1, 0) == nil {
		t.Fatal("a restriction lapsed while its owner was down")
	}
	// Back up: as pushed again.
	h.s.OwnerUp("acme_quota", 1)
	if h.s.Availability("acme.backup").Access != featuregate.Full || len(h.s.Banners(true)) != 0 {
		t.Fatal("the grant stayed clamped after the owner came back")
	}
}

func TestTheGraceIsBoundedAndTheSiteMayOnlyLowerIt(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pushed  int
		siteMax time.Duration
		want    time.Duration
	}{
		{"the core's default", 0, 0, DefaultDownGrace},
		{"what the owner pushed", 600, 0, 10 * time.Minute},
		{"the longest an owner may push", extapi.MaxDownGraceS, 0, 24 * time.Hour},
		{"the site lowers it", 3600, 5 * time.Minute, 5 * time.Minute},
		{"the site cannot raise it", 600, 2 * time.Hour, 10 * time.Minute},
		{"the site cannot go below the floor", 3600, time.Second, MinDownGrace},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z"), func(c *Config) { c.MaxDownGrace = tc.siteMax }).load().run()
			body := `{"rows":[{"down_access":"read_only","feature":"acme.backup","steps":[{"access":"full"}]}]}`
			if tc.pushed != 0 {
				body = `{"down_grace_s":` + strconv.Itoa(tc.pushed) + `,` + body[1:]
			}
			h.push(pusherOf(acmeQuota), extapi.KindAvailability, 1, body)
			h.s.mu.Lock()
			got := h.s.graceLocked("acme_quota")
			h.s.mu.Unlock()
			if got != tc.want {
				t.Fatalf("grace = %v, want %v", got, tc.want)
			}
		})
	}
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load().run()
	if _, w := h.try(pusherOf(acmeQuota), extapi.KindAvailability, 1, `{"down_grace_s":30}`); w == nil || w.Code != extapi.ErrorStateRejected {
		t.Fatalf("a grace below the floor = %+v", w)
	}
}

// A restart never renews the grace: the down start is persisted, and a
// crash-looping core keeps the first one.
func TestARestartDoesNotRenewTheGrace(t *testing.T) {
	dsn := durableDSN(t)
	clk := newFakeClock("2026-12-01T00:00:00Z")
	st := openStore(t, dsn)
	h := newHarness(t, st, clk).load().run()
	h.push(pusherOf(acmeQuota), extapi.KindAvailability, 1, `{"rows":[{"down_access":"read_only","feature":"acme.backup","steps":[{"access":"full"}]}]}`)
	h.s.OwnerDown("acme_quota")
	h.stop() // the shutdown commit persists the down start
	_ = st.Close()

	for _, after := range []time.Duration{30 * time.Minute, 20 * time.Minute} {
		clk.advance(after)
		st := openStore(t, dsn)
		h := newHarness(t, st, clk).load()
		if h.s.Availability("acme.backup").Access != featuregate.Full {
			t.Fatalf("clamped at %v", after)
		}
		_ = st.Close()
	}
	clk.advance(10*time.Minute + BootExtension)
	h = newHarness(t, openStore(t, dsn), clk).load()
	if a := h.s.Availability("acme.backup"); a.Access != featuregate.ReadOnly {
		t.Fatalf("restarts renewed the grace: %+v", a)
	}
}

// A package that no longer verifies keeps its restrictions and banners;
// only its grants go.
func TestAnUnverifiedPackageKeepsItsRestrictionsAndLosesItsGrants(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load().run()
	quota := pusherOf(acmeQuota, "push:banners")
	h.push(quota, extapi.KindAvailability, 1, backupFull)
	h.push(quota, extapi.KindFleetCapacity, 1, capacityBody(1, 8))
	h.push(quota, extapi.KindBanners, 1, quotaBanners)
	broken := acmeQuota
	broken.Verified = false
	h.s.SetPackages([]Package{broken, acmeBackup, acmeOps})
	if a := h.s.Availability("acme.backup"); a.Access != featuregate.Locked || a.Notice.Code() != messages.ErrorFeatureProviderAbsent {
		t.Fatalf("grant of an unverified package: %+v", a)
	}
	if h.s.GuardNode(1, 0) == nil || len(h.s.Banners(true)) != 2 {
		t.Fatal("an unverified package's restriction or banners were dropped")
	}
	// Not installed at all: its rows are left out until uninstalled.
	h.s.SetPackages([]Package{acmeBackup})
	if h.s.GuardNode(1, 0) != nil || len(h.s.Banners(true)) != 0 {
		t.Fatal("rows of a package that is not installed are enforced")
	}
}
