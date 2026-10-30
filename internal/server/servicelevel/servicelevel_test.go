package servicelevel

import (
	"testing"
	"time"
)

func TestYields(t *testing.T) {
	busy := func(level string, awake time.Duration) Holder { return Holder{Level: level, AwakeFor: awake} }
	idle := func(level string, idle time.Duration) Holder {
		return Holder{Level: level, AwakeFor: time.Hour, IdleFor: idle}
	}
	for _, c := range []struct {
		name   string
		holder Holder
		waiter Waiter
		want   Reason
		yields bool
	}{
		{"resident never yields, even idle", idle(Resident, 24*time.Hour), Waiter{Level: Online}, "", false},
		{"unknown level reads as resident", idle("gold", 24*time.Hour), Waiter{Level: Online}, "", false},
		{"busy online keeps its cards from offline", busy(Online, time.Hour), Waiter{Level: Offline, Waited: time.Hour}, "", false},
		{"busy online keeps its cards from resident", busy(Online, time.Hour), Waiter{Level: Resident, Waited: time.Hour}, "", false},
		{"online idle five minutes gives way", idle(Online, 5*time.Minute), Waiter{Level: Offline}, ReasonIdle, true},
		{"online idle less than five minutes does not", idle(Online, 4*time.Minute), Waiter{Level: Offline}, "", false},
		{"busy offline yields to online at once", busy(Offline, time.Second), Waiter{Level: Online}, ReasonRank, true},
		{"busy offline yields to shared", busy(Offline, time.Second), Waiter{Level: Shared}, ReasonRank, true},
		{"busy offline yields to a woken resident", busy(Offline, time.Second), Waiter{Level: Resident}, ReasonRank, true},
		{"busy offline keeps its cards from another offline", busy(Offline, time.Hour), Waiter{Level: Offline, Waited: time.Hour}, "", false},
		{"offline idle ten seconds gives way to an equal", idle(Offline, 10*time.Second), Waiter{Level: Offline}, ReasonIdle, true},
		{"busy shared yields to online", busy(Shared, time.Second), Waiter{Level: Online}, ReasonRank, true},
		{"busy shared keeps its cards from offline", busy(Shared, time.Hour), Waiter{Level: Offline, Waited: time.Hour}, "", false},
		{"shared before its turn keeps its cards", busy(Shared, 119*time.Second), Waiter{Level: Shared, Waited: time.Hour}, "", false},
		{"shared after its turn gives way to an equal that waited", busy(Shared, 2*time.Minute), Waiter{Level: Shared, Waited: 30 * time.Second}, ReasonTurn, true},
		{"shared after its turn keeps its cards from an equal that just came", busy(Shared, time.Hour), Waiter{Level: Shared, Waited: 29 * time.Second}, "", false},
		{"a slow switch lengthens the turn", Holder{Level: Shared, AwakeFor: 4 * time.Minute, SwitchTime: 30 * time.Second}, Waiter{Level: Shared, Waited: time.Hour}, "", false},
		{"a slow switch's turn ends", Holder{Level: Shared, AwakeFor: 5 * time.Minute, SwitchTime: 30 * time.Second}, Waiter{Level: Shared, Waited: time.Hour}, ReasonTurn, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			got, yields := Yields(c.holder, c.waiter)
			if got != c.want || yields != c.yields {
				t.Fatalf("Yields = %q, %v; want %q, %v", got, yields, c.want, c.yields)
			}
		})
	}
}

func TestSharesCardsWith(t *testing.T) {
	for _, c := range []struct {
		a, b string
		want bool
	}{
		{Online, Online, false},
		{Resident, Offline, false},
		{Offline, Resident, false},
		{Online, Shared, true},
		{Online, Offline, true},
		{Shared, Shared, true},
		{Offline, Offline, true},
		{Shared, Online, true},
	} {
		if got := SharesCardsWith(c.a, c.b); got != c.want {
			t.Errorf("SharesCardsWith(%s, %s) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}

func TestPolicyDefaults(t *testing.T) {
	if p := For("unknown"); p.Rank != For(Resident).Rank || p.Preemptible || p.IdleRelease != 0 {
		t.Fatalf("unknown level policy = %+v", p)
	}
	if For(Online).Rank <= For(Shared).Rank || For(Shared).Rank <= For(Offline).Rank {
		t.Fatal("ranks out of order")
	}
	if Turn(For(Shared), 0) != 2*time.Minute || Turn(For(Shared), time.Minute) != 10*time.Minute {
		t.Fatal("turn does not follow the switch time")
	}
	for _, level := range []string{Resident, Online, Shared, Offline} {
		if !Valid(level) || For(level).HoldMax <= 0 {
			t.Fatalf("%s: valid=%v hold=%v", level, Valid(level), For(level).HoldMax)
		}
	}
	if Valid("") || Valid("gold") {
		t.Fatal("invalid level accepted")
	}
}
