package sched

import (
	"reflect"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 8, 12, 0, 0, 0, time.UTC)

func decide(ms ...Model) []Action {
	return Decide(ms, t0, Policy{Rotation: 2 * time.Hour, Budget: 1.0})
}

func TestSmallestFirst(t *testing.T) {
	got := decide(
		Model{Name: "qwen", GPU: 0.95, Ready: 10},
		Model{Name: "pixai", GPU: 0.15, Ready: 3},
		Model{Name: "deepseek-ocr", GPU: 0.32, Ready: 5})
	if !reflect.DeepEqual(got, []Action{{Start, "pixai"}}) {
		t.Errorf("got %v, want start pixai", got)
	}
}

func TestOneStartAtATime(t *testing.T) {
	got := decide(
		Model{Name: "pixai", GPU: 0.15, State: Starting},
		Model{Name: "deepseek-ocr", GPU: 0.32, Ready: 5})
	if len(got) != 0 {
		t.Errorf("got %v, want nothing while pixai starts", got)
	}
}

func TestPacksWhatFits(t *testing.T) {
	// ocr (0.32) is up; joycaption (0.6) fits beside it, qwen doesn't
	got := decide(
		Model{Name: "deepseek-ocr", GPU: 0.32, State: Up, UpSince: t0, Ready: 4},
		Model{Name: "joycaption", GPU: 0.6, Ready: 2},
		Model{Name: "qwen", GPU: 0.95, Ready: 9})
	if !reflect.DeepEqual(got, []Action{{Start, "joycaption"}}) {
		t.Errorf("got %v, want start joycaption", got)
	}
}

func TestIdleMakesRoom(t *testing.T) {
	got := decide(
		Model{Name: "deepseek-ocr", GPU: 0.32, State: Up, UpSince: t0},
		Model{Name: "joycaption", GPU: 0.6, State: Up, UpSince: t0},
		Model{Name: "qwen", GPU: 0.95, Ready: 9})
	if !reflect.DeepEqual(got, []Action{{Stop, "joycaption"}, {Stop, "deepseek-ocr"}}) {
		t.Errorf("got %v, want both idle stopped", got)
	}
}

func TestBusyModelKeepsTheCardUntilRotation(t *testing.T) {
	gemma := Model{Name: "gemma", GPU: 0.95, State: Up, Ready: 50, Inflight: 8}
	qwen := Model{Name: "qwen", GPU: 0.95, Ready: 3}

	gemma.UpSince = t0.Add(-time.Hour) // up an hour: keeps going
	if got := decide(gemma, qwen); len(got) != 0 {
		t.Errorf("an hour in: got %v, want nothing", got)
	}
	gemma.UpSince = t0.Add(-3 * time.Hour) // past the rotation period: gives way
	if got := decide(gemma, qwen); !reflect.DeepEqual(got, []Action{{Drain, "gemma"}}) {
		t.Errorf("three hours in: got %v, want drain gemma", got)
	}
	// draining, with requests in flight: wait; once they're done: stop
	gemma.State = Draining
	if got := decide(gemma, qwen); len(got) != 0 {
		t.Errorf("draining with inflight: got %v, want nothing", got)
	}
	gemma.Inflight = 0
	if got := decide(gemma, qwen); !reflect.DeepEqual(got, []Action{{Stop, "gemma"}, {Start, "qwen"}}) {
		t.Errorf("drained: got %v, want stop gemma, start qwen", got)
	}
}

func TestNoRotationWithoutSomeoneWaiting(t *testing.T) {
	gemma := Model{Name: "gemma", GPU: 0.95, State: Up, Ready: 50, Inflight: 8, UpSince: t0.Add(-10 * time.Hour)}
	if got := decide(gemma, Model{Name: "qwen", GPU: 0.95}); len(got) != 0 {
		t.Errorf("got %v: nothing else waits, gemma should keep the card", got)
	}
}

func TestIdleModelStops(t *testing.T) {
	pol := Policy{Rotation: 2 * time.Hour, Idle: 10 * time.Minute, Budget: 1.0}
	quiet := Model{Name: "qwen", GPU: 0.95, State: Up, UpSince: t0.Add(-time.Hour), BusyAt: t0.Add(-11 * time.Minute)}
	got := Decide([]Model{quiet}, t0, pol)
	if len(got) != 1 || got[0] != (Action{Stop, "qwen"}) {
		t.Errorf("idle 11m with Idle=10m: %v, want qwen stopped", got)
	}
	recent := quiet
	recent.BusyAt = t0.Add(-9 * time.Minute)
	if got := Decide([]Model{recent}, t0, pol); len(got) != 0 {
		t.Errorf("idle 9m: %v, want nothing", got)
	}
	working := quiet
	working.Inflight = 1
	if got := Decide([]Model{working}, t0, pol); len(got) != 0 {
		t.Errorf("a model with a request in flight: %v, want nothing", got)
	}
	ready := quiet
	ready.Ready = 3
	if got := Decide([]Model{ready}, t0, pol); len(got) != 0 {
		t.Errorf("a model with ready jobs: %v, want nothing", got)
	}
	if got := Decide([]Model{quiet}, t0, Policy{Rotation: 2 * time.Hour, Budget: 1.0}); len(got) != 0 {
		t.Errorf("Idle 0 (never): %v, want nothing", got)
	}
	// its room counts as free in the same round: the waiting model starts
	gemma := Model{Name: "gemma", GPU: 0.9, State: Down, Ready: 2}
	got = Decide([]Model{gemma, quiet}, t0, pol)
	if len(got) != 2 || got[0] != (Action{Stop, "qwen"}) || got[1] != (Action{Start, "gemma"}) {
		t.Errorf("idle qwen, waiting gemma: %v, want qwen stopped and gemma started", got)
	}
	// Decide is pure: the caller's models are untouched
	ms := []Model{quiet}
	Decide(ms, t0, pol)
	if ms[0].State != Up {
		t.Error("Decide changed its argument")
	}
}
