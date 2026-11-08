package sched

import (
	"context"
	"encoding/json"
	"sort"
	"sync"
	"time"

	"imgrec/internal/pipeline"
	"imgrec/internal/runner"
	"imgrec/internal/spec"
)

// Scheduler applies Decide to real runners and feeds them jobs.
type Scheduler struct {
	P         *pipeline.Pipeline
	NewRunner func(*spec.Spec) (runner.Runner, error)
	Rotation  time.Duration // a busy model gives way after this, if others wait
	Budget    float64       // share of the card to fill (1.0)
	Grace     time.Duration // between SIGTERM and SIGKILL on stop
	Tick      time.Duration
	Logf      func(format string, a ...any)

	mu      sync.Mutex
	slots   map[string]*slot
	running map[string]bool // model \x00 sha being executed
	cursor  map[string]int  // per model: round-robin position over clients
}

type slot struct {
	spec       *spec.Spec
	runner     runner.Runner
	state      State
	upSince    time.Time
	inflight   int
	startFails int
	broken     bool
}

func gpuOf(s *spec.Spec) float64 {
	n, _ := s.Runner["gpu"].(json.Number)
	f, _ := n.Float64()
	return f
}

func concurrency(s *spec.Spec) int {
	for _, k := range []string{"max_num_seqs", "batch"} {
		if n, ok := s.Runner[k].(json.Number); ok {
			v, _ := n.Int64()
			if k == "batch" {
				v *= 2 // keep a worker's next batch queued behind this one
			}
			if v > 0 {
				return int(v)
			}
		}
	}
	return 4
}

func (s *Scheduler) logf(format string, a ...any) {
	if s.Logf != nil {
		s.Logf(format, a...)
	}
}

// Run schedules until ctx ends -- or, with untilDrained, until there's
// nothing left to run, and then stops every runner.
func (s *Scheduler) Run(ctx context.Context, untilDrained bool) error {
	if s.Budget == 0 {
		s.Budget = 1.0
	}
	if s.Tick == 0 {
		s.Tick = 200 * time.Millisecond
	}
	if s.Grace == 0 {
		s.Grace = 30 * time.Second
	}
	s.slots, s.running, s.cursor = map[string]*slot{}, map[string]bool{}, map[string]int{}
	for name, sp := range s.P.Specs {
		s.slots[name] = &slot{spec: sp}
	}
	defer s.stopAll()
	tick := time.NewTicker(s.Tick)
	defer tick.Stop()
	for {
		idle, err := s.round(ctx)
		if err != nil {
			return err
		}
		if untilDrained && idle {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-tick.C:
		}
	}
}

// round is one pass: decide, apply, dispatch. idle is true when there is
// nothing to run, nothing running and nothing starting.
func (s *Scheduler) round(ctx context.Context) (bool, error) {
	pending, waiting, err := s.P.Pending()
	if err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()

	var models []Model
	busy := waiting > 0
	for name, sl := range s.slots {
		if sl.state == Up && !sl.runner.Alive() {
			s.logf("%s: runner died", name)
			sl.state = Down
		}
		ready := 0
		for _, j := range pending[name] {
			if !s.running[name+"\x00"+j.SHA] {
				ready++
			}
		}
		if sl.broken {
			ready = 0
		}
		if ready > 0 || sl.inflight > 0 || sl.state == Starting {
			busy = true
		}
		models = append(models, Model{Name: name, GPU: gpuOf(sl.spec), State: sl.state,
			UpSince: sl.upSince, Inflight: sl.inflight, Ready: ready})
	}
	sort.Slice(models, func(i, j int) bool { return models[i].Name < models[j].Name })

	for _, a := range Decide(models, time.Now(), s.Rotation, s.Budget) {
		sl := s.slots[a.Model]
		switch a.Kind {
		case Start:
			s.start(ctx, a.Model, sl)
		case Stop:
			s.logf("%s: stopping", a.Model)
			r := sl.runner
			sl.state, sl.runner = Down, nil
			go r.Stop(s.Grace)
		case Drain:
			s.logf("%s: up %s with others waiting: draining (rotation)", a.Model,
				time.Since(sl.upSince).Round(time.Second))
			sl.state = Draining
		}
	}
	for name, sl := range s.slots {
		if sl.state == Up {
			s.dispatch(ctx, name, sl, pending[name])
		}
	}
	return !busy, nil
}

func (s *Scheduler) start(ctx context.Context, name string, sl *slot) {
	r, err := s.NewRunner(sl.spec)
	if err != nil {
		s.logf("%s: %v", name, err)
		sl.broken = true
		return
	}
	sl.state, sl.runner = Starting, r
	s.logf("%s: starting", name)
	go func() {
		t := time.Now()
		err := r.Start(ctx)
		s.mu.Lock()
		defer s.mu.Unlock()
		if err != nil {
			sl.state, sl.runner = Down, nil
			sl.startFails++
			s.logf("%s: failed to start (%d): %v", name, sl.startFails, err)
			if sl.startFails >= 2 {
				sl.broken = true
				s.logf("%s: giving up on it", name)
			}
			return
		}
		sl.state, sl.upSince = Up, time.Now()
		s.logf("%s: up after %s", name, time.Since(t).Round(time.Millisecond))
	}()
}

// dispatch sends ready jobs up to the model's concurrency, taking turns
// between clients.
func (s *Scheduler) dispatch(ctx context.Context, name string, sl *slot, jobs []pipeline.Job) {
	free := concurrency(sl.spec) - sl.inflight
	if free <= 0 || len(jobs) == 0 {
		return
	}
	// each client's jobs, oldest first; a job several clients asked for is
	// listed under each and runs once
	byClient := map[string][]pipeline.Job{}
	for _, j := range jobs {
		for _, c := range j.Clients {
			byClient[c] = append(byClient[c], j)
		}
	}
	clients := make([]string, 0, len(byClient))
	for c := range byClient {
		clients = append(clients, c)
	}
	sort.Strings(clients)
	next := map[string]int{}
	start := s.cursor[name]
	for free > 0 {
		progressed := false
		for i := range clients {
			c := clients[(start+i)%len(clients)]
			for next[c] < len(byClient[c]) {
				j := byClient[c][next[c]]
				next[c]++
				key := name + "\x00" + j.SHA
				if s.running[key] {
					continue
				}
				s.running[key] = true
				sl.inflight++
				free--
				progressed = true
				go s.execute(ctx, name, sl, sl.runner, j.SHA)
				break
			}
			if free == 0 {
				s.cursor[name] = (start + i + 1) % len(clients)
				return
			}
		}
		if !progressed {
			s.cursor[name] = (start + 1) % len(clients)
			return
		}
	}
}

func (s *Scheduler) execute(ctx context.Context, name string, sl *slot, r runner.Runner, sha string) {
	err := s.P.Execute(ctx, name, sha, r.Ask)
	if err != nil {
		s.logf("%s %s: %v", name, sha[:12], err)
	}
	s.mu.Lock()
	sl.inflight--
	delete(s.running, name+"\x00"+sha)
	s.mu.Unlock()
}

func (s *Scheduler) stopAll() {
	s.mu.Lock()
	var rs []runner.Runner
	for _, sl := range s.slots {
		if sl.runner != nil {
			rs = append(rs, sl.runner)
			sl.runner, sl.state = nil, Down
		}
	}
	s.mu.Unlock()
	var wg sync.WaitGroup
	for _, r := range rs {
		wg.Add(1)
		go func() { defer wg.Done(); r.Stop(s.Grace) }()
	}
	wg.Wait()
}
