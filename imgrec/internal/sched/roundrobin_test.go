package sched

import (
	"fmt"
	"reflect"
	"testing"

	"slopbooru/imgrec/internal/pipeline"
)

func jobs(client string, n int) []pipeline.Job {
	var out []pipeline.Job
	for i := range n {
		out = append(out, pipeline.Job{SHA: fmt.Sprintf("%s%02d", client, i), Clients: []string{client}})
	}
	return out
}

func none(string) bool { return false }

func TestRoundRobin(t *testing.T) {
	// alice queued ten before bob queued two: bob still gets every other slot
	all := append(jobs("alice", 10), jobs("bob", 2)...)
	got, last := RoundRobin(all, none, "", 4)
	if want := []string{"alice00", "bob00", "alice01", "bob01"}; !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	// the next round starts after the last client served
	got, _ = RoundRobin(all[2:10:10], none, last, 2) // alice's remaining, bob done
	if want := []string{"alice02", "alice03"}; !reflect.DeepEqual(got, want) {
		t.Errorf("next round: got %v, want %v", got, want)
	}
	// starting after alice means bob goes first
	got, _ = RoundRobin(all, none, "alice", 2)
	if want := []string{"bob00", "alice00"}; !reflect.DeepEqual(got, want) {
		t.Errorf("after alice: got %v, want %v", got, want)
	}
}

func TestRoundRobinSharedAndBusy(t *testing.T) {
	shared := pipeline.Job{SHA: "shared", Clients: []string{"alice", "bob"}}
	all := []pipeline.Job{shared, {SHA: "a1", Clients: []string{"alice"}}, {SHA: "b1", Clients: []string{"bob"}}}
	got, _ := RoundRobin(all, none, "", 10)
	if want := []string{"shared", "b1", "a1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("a job two clients asked for runs once: got %v, want %v", got, want)
	}
	got, _ = RoundRobin(all, func(sha string) bool { return sha == "shared" }, "", 10)
	if want := []string{"a1", "b1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("running jobs are skipped: got %v, want %v", got, want)
	}
}
