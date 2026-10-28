package automation

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"
)

// fakeWorld is the automation's view of the platform: schedules, idle sleep
// settings, waiting wakes and the gateway's request clock, and a record of
// every order given. Orders can be made to fail.
type fakeWorld struct {
	schedules []Schedule
	idle      []IdleDeployment
	waits     []WaitingWake
	lastReq   map[uint]time.Time

	sleepErr   error
	wakeResult WakeResult
	wakeErr    error
	fits       map[uint][]string // WakeWaiting answers these as woken
	readErr    error

	orders []string
	events []string
	params []map[string]any
	fired  map[uint]time.Time
	ended  [][]string
	held   []bool
}

func newFakeWorld() *fakeWorld {
	return &fakeWorld{lastReq: map[uint]time.Time{}, fits: map[uint][]string{}, fired: map[uint]time.Time{}}
}

func (w *fakeWorld) Schedules() ([]Schedule, error) {
	if w.readErr != nil {
		return nil, w.readErr
	}
	out := append([]Schedule(nil), w.schedules...)
	for i := range out {
		if at, ok := w.fired[out[i].ID]; ok {
			out[i].LastFiredAt = at
		}
	}
	return out, nil
}
func (w *fakeWorld) IdleDeployments() ([]IdleDeployment, error) { return w.idle, w.readErr }
func (w *fakeWorld) WaitingWakes() ([]WaitingWake, error)       { return w.waits, w.readErr }
func (w *fakeWorld) LastRequestAt(id uint) (time.Time, bool) {
	at, ok := w.lastReq[id]
	return at, ok
}
func (w *fakeWorld) Sleep(id uint, level int, held bool) error {
	w.orders = append(w.orders, fmt.Sprintf("sleep %d L%d", id, level))
	w.held = append(w.held, held)
	return w.sleepErr
}
func (w *fakeWorld) Wake(id uint) (WakeResult, error) {
	w.orders = append(w.orders, fmt.Sprintf("wake %d", id))
	return w.wakeResult, w.wakeErr
}
func (w *fakeWorld) WakeWaiting(id uint, instances []string) ([]string, error) {
	w.orders = append(w.orders, fmt.Sprintf("wake-waiting %d %v", id, instances))
	return w.fits[id], nil
}
func (w *fakeWorld) EndWaiting(id uint, instances []string) error {
	w.ended = append(w.ended, instances)
	return nil
}
func (w *fakeWorld) ScaleReplicas(id uint, n int) error {
	w.orders = append(w.orders, fmt.Sprintf("replicas %d=%d", id, n))
	return nil
}
func (w *fakeWorld) MarkFired(id uint, at time.Time) error {
	if at.After(w.fired[id]) {
		w.fired[id] = at
	}
	return nil
}
func (w *fakeWorld) Record(id uint, code string, params map[string]any) {
	w.events = append(w.events, fmt.Sprintf("%d %s", id, code))
	w.params = append(w.params, params)
}

type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }

func newEvaluator(w *fakeWorld, start time.Time) (*Evaluator, *clock) {
	c := &clock{t: start}
	e := New(w, w, nil)
	e.now = c.now
	return e, c
}

var shanghai = time.FixedZone("Asia/Shanghai", 8*3600)

func weekday(days ...time.Weekday) int {
	mask := 0
	for _, d := range days {
		mask |= 1 << int(d)
	}
	return mask
}

func TestLatestOccurrenceHonoursWeekdaysAndTheZone(t *testing.T) {
	s := Schedule{Weekdays: weekday(time.Monday, time.Friday), Minute: 21 * 60, Location: shanghai}
	// Wednesday 2026-10-28 10:00 in Shanghai: the latest is Monday 21:00.
	now := time.Date(2026, 10, 28, 10, 0, 0, 0, shanghai)
	at, ok := s.LatestOccurrence(now)
	if !ok || !at.Equal(time.Date(2026, 10, 26, 21, 0, 0, 0, shanghai)) {
		t.Fatalf("latest = %v %v", at, ok)
	}
	// Friday 21:00 exactly is its own occurrence.
	now = time.Date(2026, 10, 30, 21, 0, 0, 0, shanghai)
	if at, ok := s.LatestOccurrence(now); !ok || !at.Equal(now) {
		t.Fatalf("at the minute = %v %v", at, ok)
	}
	// The same instant seen from UTC gives the same occurrence.
	if at, _ := s.LatestOccurrence(now.UTC()); !at.Equal(now) {
		t.Fatalf("zone of now leaked into the schedule: %v", at)
	}
	if _, ok := (Schedule{Minute: 60, Location: shanghai}).LatestOccurrence(now); ok {
		t.Fatal("a schedule with no weekday named an occurrence")
	}
	occ := s.Occurrences(time.Date(2026, 10, 26, 0, 0, 0, 0, shanghai), time.Date(2026, 11, 2, 23, 0, 0, 0, shanghai))
	if len(occ) != 3 || occ[0].Weekday() != time.Monday || occ[1].Weekday() != time.Friday || occ[2].Weekday() != time.Monday {
		t.Fatalf("occurrences = %v", occ)
	}
}

// An occurrence runs once; a manual order made after it is not overridden on
// the next tick; the next occurrence runs again.
func TestAScheduledOrderRunsOncePerOccurrence(t *testing.T) {
	w := newFakeWorld()
	start := time.Date(2026, 10, 26, 20, 59, 0, 0, shanghai)
	w.schedules = []Schedule{{ID: 1, DeploymentID: 7, Action: "sleep", SleepLevel: 2, Weekdays: AllWeekdays, Minute: 21 * 60, Location: shanghai, LastFiredAt: start.Add(-time.Hour)}}
	e, c := newEvaluator(w, start)
	e.Tick()
	if len(w.orders) != 0 {
		t.Fatalf("ordered before the minute: %v", w.orders)
	}
	c.t = start.Add(2 * time.Minute)
	e.Tick()
	e.Tick()
	c.t = c.t.Add(time.Hour)
	e.Tick()
	if !slices.Equal(w.orders, []string{"sleep 7 L2"}) || !slices.Equal(w.held, []bool{true}) {
		t.Fatalf("orders = %v held = %v, want one held sleep (a request does not undo a schedule)", w.orders, w.held)
	}
	if !slices.Equal(w.events, []string{"7 AUTO_SLEEP"}) || w.params[0]["trigger"] != TriggerSchedule || w.params[0]["schedule_id"] != uint(1) {
		t.Fatalf("events = %v %v", w.events, w.params)
	}
	c.t = start.Add(24*time.Hour + 2*time.Minute)
	e.Tick()
	if len(w.orders) != 2 {
		t.Fatalf("next day's occurrence did not run: %v", w.orders)
	}
}

// A server that was down across an occurrence runs it when it starts, if the
// occurrence is recent enough; an older one is only marked handled.
func TestAMissedOccurrenceRunsOnlyWithinTheCatchUpWindow(t *testing.T) {
	w := newFakeWorld()
	monday := time.Date(2026, 10, 26, 8, 50, 0, 0, shanghai)
	w.schedules = []Schedule{{ID: 1, DeploymentID: 7, Action: "wake", Weekdays: AllWeekdays, Minute: 8*60 + 50, Location: shanghai, LastFiredAt: monday.Add(-24 * time.Hour)}}
	w.wakeResult = WakeResult{Woken: []string{"a"}}
	e, _ := newEvaluator(w, monday.Add(3*time.Hour))
	e.Tick()
	if !slices.Equal(w.orders, []string{"wake 7"}) {
		t.Fatalf("missed occurrence 3h ago = %v", w.orders)
	}

	w = newFakeWorld()
	w.schedules = []Schedule{{ID: 1, DeploymentID: 7, Action: "wake", Weekdays: AllWeekdays, Minute: 8*60 + 50, Location: shanghai, LastFiredAt: monday.Add(-24 * time.Hour)}}
	e, _ = newEvaluator(w, monday.Add(13*time.Hour))
	e.Tick()
	if len(w.orders) != 0 || !w.fired[1].Equal(monday) {
		t.Fatalf("stale occurrence: orders %v fired %v", w.orders, w.fired[1])
	}
}

// Two orders of one deployment due at once (the server was down across both)
// run oldest first, so the newer one stands.
func TestDueOrdersRunOldestFirst(t *testing.T) {
	w := newFakeWorld()
	day := time.Date(2026, 10, 26, 0, 0, 0, 0, shanghai)
	w.schedules = []Schedule{
		{ID: 1, DeploymentID: 7, Action: "wake", Weekdays: AllWeekdays, Minute: 9 * 60, Location: shanghai, LastFiredAt: day},
		{ID: 2, DeploymentID: 7, Action: "sleep", SleepLevel: 1, Weekdays: AllWeekdays, Minute: 8 * 60, Location: shanghai, LastFiredAt: day},
	}
	e, _ := newEvaluator(w, day.Add(10*time.Hour))
	e.Tick()
	if !slices.Equal(w.orders, []string{"sleep 7 L1", "wake 7"}) {
		t.Fatalf("orders = %v", w.orders)
	}
}

// A failed order is recorded with its reason and code, and not retried on the
// next tick.
func TestAFailedScheduledOrderIsRecordedOnce(t *testing.T) {
	w := newFakeWorld()
	start := time.Date(2026, 10, 26, 21, 1, 0, 0, shanghai)
	w.schedules = []Schedule{{ID: 3, DeploymentID: 9, Action: "sleep", SleepLevel: 1, Weekdays: AllWeekdays, Minute: 21 * 60, Location: shanghai, LastFiredAt: start.Add(-time.Hour)}}
	w.sleepErr = codedErr{code: "SLEEP_HOST_MEMORY_INSUFFICIENT"}
	e, _ := newEvaluator(w, start)
	e.Tick()
	e.Tick()
	if len(w.orders) != 1 || !slices.Equal(w.events, []string{"9 SCHEDULE_ACTION_FAILED"}) {
		t.Fatalf("orders %v events %v", w.orders, w.events)
	}
	if p := w.params[0]; p["reason_code"] != "SLEEP_HOST_MEMORY_INSUFFICIENT" || p["action"] != "sleep" {
		t.Fatalf("failure params = %v", p)
	}
}

// A scheduled wake that does not fit leaves the rest waiting, and says so.
func TestAScheduledWakeThatDoesNotFitWaits(t *testing.T) {
	w := newFakeWorld()
	start := time.Date(2026, 10, 26, 9, 0, 30, 0, shanghai)
	w.schedules = []Schedule{{ID: 1, DeploymentID: 7, Action: "wake", Weekdays: AllWeekdays, Minute: 9 * 60, Location: shanghai, LastFiredAt: start.Add(-time.Hour)}}
	w.wakeResult = WakeResult{Woken: []string{"a"}, Waiting: []string{"b"}}
	e, _ := newEvaluator(w, start)
	e.Tick()
	if !slices.Equal(w.events, []string{"7 AUTO_WAKE", "7 WAKE_WAITING"}) {
		t.Fatalf("events = %v", w.events)
	}
}

func TestAScheduledReplicaCountIsApplied(t *testing.T) {
	w := newFakeWorld()
	start := time.Date(2026, 10, 26, 9, 0, 30, 0, shanghai)
	w.schedules = []Schedule{{ID: 1, DeploymentID: 7, Action: "replicas", Replicas: 4, Weekdays: AllWeekdays, Minute: 9 * 60, Location: shanghai, LastFiredAt: start.Add(-time.Hour)}}
	e, _ := newEvaluator(w, start)
	e.Tick()
	if !slices.Equal(w.orders, []string{"replicas 7=4"}) || !slices.Equal(w.events, []string{"7 AUTO_SCALE"}) {
		t.Fatalf("orders %v events %v", w.orders, w.events)
	}
}

// Idle sleep waits a whole idle period from when the server started watching,
// counts from the last user request, and never sleeps an asleep deployment.
func TestIdleSleepCountsFromTheLastRequest(t *testing.T) {
	w := newFakeWorld()
	start := time.Date(2026, 10, 26, 10, 0, 0, 0, shanghai)
	w.idle = []IdleDeployment{{ID: 5, AfterSec: 900, Level: 2, Awake: true}}
	e, c := newEvaluator(w, start)
	e.Tick()
	c.t = start.Add(10 * time.Minute)
	w.lastReq[5] = start.Add(8 * time.Minute)
	e.Tick()
	c.t = start.Add(20 * time.Minute)
	e.Tick()
	if len(w.orders) != 0 {
		t.Fatalf("slept before 15 idle minutes: %v", w.orders)
	}
	c.t = start.Add(23 * time.Minute)
	e.Tick()
	if !slices.Equal(w.orders, []string{"sleep 5 L2"}) || w.params[0]["trigger"] != TriggerIdle || w.params[0]["idle_sec"] != 900 {
		t.Fatalf("orders %v params %v", w.orders, w.params)
	}
	if !slices.Equal(w.held, []bool{false}) {
		t.Fatalf("held = %v: an idle sleep must wake on the next request", w.held)
	}
	// Asleep now: nothing more happens.
	w.idle[0].Awake = false
	c.t = c.t.Add(time.Hour)
	e.Tick()
	if len(w.orders) != 1 {
		t.Fatalf("asleep deployment slept again: %v", w.orders)
	}
	// Woken by hand: a full idle period again before it sleeps.
	w.idle[0].Awake = true
	woke := c.t
	e.Tick()
	c.t = woke.Add(14 * time.Minute)
	e.Tick()
	if len(w.orders) != 1 {
		t.Fatalf("slept right after a wake: %v", w.orders)
	}
	c.t = woke.Add(15 * time.Minute)
	e.Tick()
	if len(w.orders) != 2 {
		t.Fatalf("did not sleep a full period after the wake: %v", w.orders)
	}
}

// A refused idle sleep is recorded and not retried until another idle period
// passes.
func TestARefusedIdleSleepWaitsAnotherPeriod(t *testing.T) {
	w := newFakeWorld()
	start := time.Date(2026, 10, 26, 10, 0, 0, 0, shanghai)
	w.idle = []IdleDeployment{{ID: 5, AfterSec: 60, Level: 1, Awake: true}}
	w.sleepErr = errors.New("boom")
	e, c := newEvaluator(w, start)
	e.Tick()
	c.t = start.Add(61 * time.Second)
	e.Tick()
	c.t = start.Add(90 * time.Second)
	e.Tick()
	if len(w.orders) != 1 || !slices.Equal(w.events, []string{"5 SCHEDULE_ACTION_FAILED"}) {
		t.Fatalf("orders %v events %v", w.orders, w.events)
	}
	c.t = start.Add(122 * time.Second)
	e.Tick()
	if len(w.orders) != 2 {
		t.Fatalf("not retried after another period: %v", w.orders)
	}
}

// A waiting wake wakes what fits, and gives up after the wait limit.
func TestAWaitingWakeWakesWhenItFitsAndGivesUpInTime(t *testing.T) {
	w := newFakeWorld()
	start := time.Date(2026, 10, 26, 10, 0, 0, 0, shanghai)
	w.waits = []WaitingWake{{DeploymentID: 4, Instances: []string{"x", "y"}, Since: start}}
	e, c := newEvaluator(w, start.Add(time.Minute))
	e.Tick()
	if len(w.events) != 0 {
		t.Fatalf("nothing fits yet, events = %v", w.events)
	}
	w.fits[4] = []string{"x"}
	e.Tick()
	if !slices.Equal(w.events, []string{"4 AUTO_WAKE"}) || w.params[0]["trigger"] != TriggerWakeWaiting {
		t.Fatalf("events %v params %v", w.events, w.params)
	}
	w.waits[0].Instances = []string{"y"}
	w.fits[4] = nil
	c.t = start.Add(DefaultWakeWaitLimit)
	e.Tick()
	if len(w.ended) != 1 || !slices.Equal(w.ended[0], []string{"y"}) || w.events[len(w.events)-1] != "4 WAKE_WAIT_TIMEOUT" {
		t.Fatalf("ended %v events %v", w.ended, w.events)
	}
}

// A read that fails skips that part of the tick and changes nothing.
func TestAFailedReadChangesNothing(t *testing.T) {
	w := newFakeWorld()
	w.readErr = errors.New("database is locked")
	w.schedules = []Schedule{{ID: 1, DeploymentID: 7, Action: "sleep", SleepLevel: 2, Weekdays: AllWeekdays, Minute: 0, Location: shanghai}}
	e, _ := newEvaluator(w, time.Date(2026, 10, 26, 10, 0, 0, 0, shanghai))
	e.Tick()
	if len(w.orders) != 0 || len(w.events) != 0 {
		t.Fatalf("orders %v events %v", w.orders, w.events)
	}
}

type codedErr struct{ code string }

func (e codedErr) Error() string { return "refused: " + e.code }
func (e codedErr) Code() string  { return e.code }
