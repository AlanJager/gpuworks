package batch

import (
	"errors"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The dictionary states what the create checks: the default window is the one
// a batch gets unasked, every window it offers parses and none outside does,
// the statuses are every status once, and the models are the lookup's list.
func TestDictionaryIsWhatTheCreateChecks(t *testing.T) {
	r := newRig(t, Config{Models: platformModels, OutputRetention: 7 * 24 * time.Hour}, nil)
	d, err := r.svc.Dictionary()
	if err != nil {
		t.Fatal(err)
	}
	if got, ok := ParseWindow(CompletionWindow); !ok || got != time.Duration(d.DefaultWindowHours)*time.Hour {
		t.Fatalf("default window %dh, but a batch asking for none gets %s", d.DefaultWindowHours, CompletionWindow)
	}
	for _, hours := range []int{d.MinWindowHours, d.MaxWindowHours} {
		if _, ok := ParseWindow(itoaHours(hours)); !ok {
			t.Errorf("the offered bound %dh is refused", hours)
		}
	}
	for _, hours := range []int{d.MinWindowHours - 1, d.MaxWindowHours + 1} {
		if _, ok := ParseWindow(itoaHours(hours)); ok {
			t.Errorf("%dh, outside the offered bounds, is accepted", hours)
		}
	}
	if d.MaxFileBytes != MaxFileBytes || d.MaxFileLines != MaxFileLines || !reflect.DeepEqual(d.Endpoints, Endpoints) || d.OutputRetention != 7*24*time.Hour {
		t.Fatalf("dictionary = %+v", d)
	}
	all := []string{StatusValidating, StatusInProgress, StatusFinalizing, StatusCompleted, StatusFailed, StatusExpired, StatusCancelling, StatusCancelled}
	got := append([]string(nil), d.Statuses...)
	sort.Strings(got)
	sort.Strings(all)
	if !reflect.DeepEqual(got, all) {
		t.Fatalf("statuses = %v", d.Statuses)
	}
	for i, status := range d.Statuses {
		if Terminal(status) && i < len(d.Statuses)-1 && !Terminal(d.Statuses[i+1]) {
			t.Fatalf("a moving status follows a terminal one: %v", d.Statuses)
		}
	}
	want, _ := platformModels.List()
	if !reflect.DeepEqual(d.Models, want) {
		t.Fatalf("models = %+v, want the lookup's %+v", d.Models, want)
	}
}

func itoaHours(n int) string { return strconv.Itoa(n) + "h" }

// Without a lookup no model is listed (and none is checked); a failing lookup
// fails the dictionary rather than listing nothing.
func TestDictionaryModelsWithoutOrFailingALookup(t *testing.T) {
	r := newRig(t, Config{}, nil)
	d, err := r.svc.Dictionary()
	if err != nil || d.Models == nil || len(d.Models) != 0 {
		t.Fatalf("without a lookup = %+v err=%v", d.Models, err)
	}
	r = newRig(t, Config{Models: fakeModels{listErr: errors.New("store is down")}}, nil)
	if _, err := r.svc.Dictionary(); err == nil {
		t.Fatal("a failing list gave a dictionary")
	}
}

// Every state but unknown is known; the ones that wait for someone to act are
// held, offline, failed and stopped; ranking follows ModelStates, unknown last.
func TestModelStatesKnownWaitAndRank(t *testing.T) {
	waits := map[ModelState]bool{ModelSleepHeld: true, ModelOffline: true, ModelFailed: true, ModelStopped: true}
	for _, state := range ModelStates {
		if !state.Known() || state.Waits() != waits[state] {
			t.Fatalf("state %d: known=%v waits=%v", state, state.Known(), state.Waits())
		}
	}
	if ModelUnknown.Known() || ModelUnknown.Waits() {
		t.Fatal("unknown")
	}
	order := append(append([]ModelState{}, ModelStates...), ModelUnknown)
	want := []ModelState{ModelRunning, ModelAsleep, ModelStarting, ModelSleepHeld, ModelOffline, ModelFailed, ModelStopped, ModelUnknown}
	if !reflect.DeepEqual(order, want) {
		t.Fatalf("order = %v", order)
	}
	for i := 0; i+1 < len(order); i++ {
		if !order[i].Better(order[i+1]) || order[i+1].Better(order[i]) {
			t.Fatalf("%v should rank above %v", order[i], order[i+1])
		}
	}
	r := newRig(t, Config{Models: fakeModels{states: map[string]ModelState{"napping": ModelAsleep, "booting": ModelStarting}}}, nil)
	for _, name := range []string{"napping", "booting"} {
		if warnings, err := r.svc.checkModel(name, EndpointChat); err != nil || len(warnings) != 0 {
			t.Fatalf("%s = %v %v", name, warnings, err)
		}
	}
}

// A model that is offline, failed or stopped is accepted with a warning that
// names its state; one whose deployments do not expose the batch's endpoint
// is refused, naming the endpoint.
func TestCheckModelWaitsAndEndpoint(t *testing.T) {
	r := newRig(t, Config{Models: fakeModels{
		states:    map[string]ModelState{"gone": ModelOffline, "broken": ModelFailed, "parked": ModelStopped, "embed": ModelRunning},
		endpoints: map[string][]string{"embed": {EndpointEmbedding}},
	}}, nil)
	for name, state := range map[string]ModelState{"gone": ModelOffline, "broken": ModelFailed, "parked": ModelStopped} {
		warnings, err := r.svc.checkModel(name, EndpointChat)
		if err != nil || len(warnings) != 1 {
			t.Fatalf("%s = %+v %v", name, warnings, err)
		}
		w := warnings[0]
		if w.Code != WarningModelWaits || w.Param != ParamModel || w.State != state || !reflect.DeepEqual(w.Models, []string{name}) || !strings.Contains(w.Message, waitWords[state]) {
			t.Fatalf("%s warning = %+v", name, w)
		}
	}
	_, err := r.svc.checkModel("embed", EndpointChat)
	var be *Error
	if !errors.As(err, &be) || be.Status != 400 || be.Code != CodeModelEndpoint || be.Param != ParamModel || be.Model != "embed" || be.Endpoint != EndpointChat {
		t.Fatalf("an embedding model for chat = %#v", err)
	}
	if warnings, err := r.svc.checkModel("embed", EndpointEmbedding); err != nil || len(warnings) != 0 {
		t.Fatalf("an embedding model for embeddings = %v %v", warnings, err)
	}
}
