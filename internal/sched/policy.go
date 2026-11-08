// Package sched decides which models are loaded and feeds them jobs.
//
// The constraint is VRAM, not job count, and loading takes minutes. The
// policy (Decide) is a pure function of the models' states, so it tests
// without processes; the driver (Scheduler) applies its decisions.
package sched

import (
	"sort"
	"time"
)

type State int

const (
	Down State = iota
	Starting
	Up
	Draining // finishing what's in flight, taking nothing new, then stops
)

func (s State) String() string { return [...]string{"down", "starting", "up", "draining"}[s] }

// Model is what the policy sees of one model.
type Model struct {
	Name     string
	GPU      float64 // share of the card its runner takes
	State    State
	UpSince  time.Time
	Inflight int // requests being answered
	Ready    int // jobs that could be sent now
}

type ActionKind int

const (
	Start ActionKind = iota
	Stop
	Drain
)

func (k ActionKind) String() string { return [...]string{"start", "stop", "drain"}[k] }

type Action struct {
	Kind  ActionKind
	Model string
}

const eps = 1e-9

// Decide is one round of the policy:
//
//  1. a draining model with nothing in flight stops;
//  2. one start at a time (vllm sizes its KV cache from memory measured
//     while starting; a neighbour starting concurrently makes it lie);
//  3. models already up are drained first (by the driver); when a model
//     with ready jobs isn't up, start the smallest such model that fits;
//  4. if none fits, stop idle models to make room;
//  5. if that isn't enough, rotate: a busy model up longer than the
//     rotation period drains, so a steady stream for one model can't
//     starve the rest. It finishes what's in flight, then stops.
func Decide(models []Model, now time.Time, rotation time.Duration, budget float64) []Action {
	var out []Action
	used, starting, draining := 0.0, false, false
	for _, m := range models {
		switch m.State {
		case Draining:
			if m.Inflight == 0 {
				out = append(out, Action{Stop, m.Name})
				continue
			}
			draining = true
			used += m.GPU
		case Starting:
			starting = true
			used += m.GPU
		case Up:
			used += m.GPU
		}
	}
	if starting {
		return out
	}
	var wanted []Model
	for _, m := range models {
		if m.State == Down && m.Ready > 0 {
			wanted = append(wanted, m)
		}
	}
	if len(wanted) == 0 {
		return out
	}
	sort.Slice(wanted, func(i, j int) bool {
		if wanted[i].GPU != wanted[j].GPU {
			return wanted[i].GPU < wanted[j].GPU
		}
		return wanted[i].Name < wanted[j].Name
	})
	for _, w := range wanted {
		if used+w.GPU <= budget+eps {
			return append(out, Action{Start, w.Name})
		}
	}
	// nothing fits: idle models make room first
	need := wanted[0].GPU - (budget - used)
	var idle []Model
	for _, m := range models {
		if m.State == Up && m.Ready == 0 && m.Inflight == 0 {
			idle = append(idle, m)
		}
	}
	sort.Slice(idle, func(i, j int) bool { return idle[i].GPU > idle[j].GPU })
	freed := 0.0
	var stops []Action
	for _, m := range idle {
		if freed >= need-eps {
			break
		}
		freed += m.GPU
		stops = append(stops, Action{Stop, m.Name})
	}
	if freed >= need-eps {
		return append(out, stops...)
	}
	if draining {
		return out // room is already being made
	}
	// rotation: the longest-up busy model gives way, if that makes room
	var busy []Model
	for _, m := range models {
		if m.State == Up && (m.Ready > 0 || m.Inflight > 0) && now.Sub(m.UpSince) >= rotation {
			busy = append(busy, m)
		}
	}
	sort.Slice(busy, func(i, j int) bool { return busy[i].UpSince.Before(busy[j].UpSince) })
	for _, m := range busy {
		if freed+m.GPU >= need-eps {
			return append(append(out, stops...), Action{Drain, m.Name})
		}
	}
	return out
}
