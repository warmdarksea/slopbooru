// Package runner starts and stops the processes that hold a model in
// VRAM. Loading takes minutes, so a runner is long-lived: started once,
// asked many times, stopped to make room.
//
//   - VLLM: `vllm serve ...`, asked over HTTP (OpenAI chat completions)
//   - Worker: a process that reads one json request per line on stdin and
//     answers one json line per request on stdout (the pixai worker)
//
// Stop takes the whole process group down: vllm forks workers, and they
// are what hold the VRAM.
package runner

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Runner is one model's process.
type Runner interface {
	Start(ctx context.Context) error // returns once it answers
	Ask(ctx context.Context, request []byte) ([]byte, error)
	Stop(grace time.Duration) error // SIGTERM the group, then SIGKILL
	Kill() error
	Alive() bool
}

// process is the shared part: a command in its own process group.
type process struct {
	cmd     *exec.Cmd
	exited  chan struct{}
	waitErr error
	log     io.Writer
}

func (p *process) start(argv []string, env []string, log io.Writer) error {
	p.cmd = exec.Command(argv[0], argv[1:]...)
	p.cmd.Env = append(os.Environ(), env...)
	p.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if log == nil {
		log = io.Discard
	}
	p.log = log
	if p.cmd.Stderr == nil {
		p.cmd.Stderr = log
	}
	if err := p.cmd.Start(); err != nil {
		return err
	}
	p.exited = make(chan struct{})
	go func() { p.waitErr = p.cmd.Wait(); close(p.exited) }()
	return nil
}

func (p *process) Alive() bool {
	if p.exited == nil {
		return false
	}
	select {
	case <-p.exited:
		return false
	default:
		return true
	}
}

func (p *process) signal(sig syscall.Signal) {
	if p.cmd != nil && p.cmd.Process != nil {
		syscall.Kill(-p.cmd.Process.Pid, sig) // the group: vllm's workers too
	}
}

func (p *process) Stop(grace time.Duration) error {
	if !p.Alive() {
		return nil
	}
	p.signal(syscall.SIGTERM)
	select {
	case <-p.exited:
	case <-time.After(grace):
		p.signal(syscall.SIGKILL)
		<-p.exited
	}
	// a worker the leader left behind still holds VRAM: make sure
	p.signal(syscall.SIGKILL)
	return nil
}

func (p *process) Kill() error {
	if !p.Alive() {
		return nil
	}
	p.signal(syscall.SIGKILL)
	<-p.exited
	return nil
}

// ---- vllm ------------------------------------------------------------------

// VLLM serves one model over HTTP on 127.0.0.1:Port.
type VLLM struct {
	process
	Argv    []string // the full command, e.g. vllm serve WEIGHTS --port N ...
	Env     []string
	Port    int
	Log     io.Writer
	Timeout time.Duration // per request
	client  *http.Client
}

func (v *VLLM) base() string { return fmt.Sprintf("http://127.0.0.1:%d", v.Port) }

func (v *VLLM) Start(ctx context.Context) error {
	v.client = &http.Client{Timeout: v.Timeout}
	if err := v.process.start(v.Argv, v.Env, v.Log); err != nil {
		return err
	}
	tick := time.NewTicker(500 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			v.Kill()
			return ctx.Err()
		case <-v.exited:
			return fmt.Errorf("exited while starting: %v", v.waitErr)
		case <-tick.C:
			r, err := http.Get(v.base() + "/health")
			if err == nil {
				r.Body.Close()
				if r.StatusCode == 200 {
					return nil
				}
			}
		}
	}
}

// Ask posts a chat-completions request and returns the reply verbatim.
func (v *VLLM) Ask(ctx context.Context, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, "POST", v.base()+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	resp, err := v.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != 200 {
		if len(out) > 400 {
			out = out[:400]
		}
		return nil, fmt.Errorf("request failed: %d %s", resp.StatusCode, strings.TrimSpace(string(out)))
	}
	return out, nil
}

// ---- stdio worker ----------------------------------------------------------

// Worker speaks json lines: it is sent {"id": N, ...request} and answers
// {"id": N, ...reply}, in any order. Ready when it prints {"ready": true}.
type Worker struct {
	process
	Argv    []string
	Env     []string
	Log     io.Writer
	Timeout time.Duration

	mu      sync.Mutex
	stdin   io.WriteCloser
	waiting map[int64]chan []byte
	next    atomic.Int64
	ready   chan struct{}
}

func (w *Worker) Start(ctx context.Context) error {
	w.waiting = map[int64]chan []byte{}
	w.ready = make(chan struct{})
	w.process.cmd = nil
	cmd := exec.Command(w.Argv[0], w.Argv[1:]...)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	w.stdin = stdin
	// start through process so it gets its own group and the exit watcher
	w.process.cmd = cmd
	cmd.Env = append(os.Environ(), w.Env...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if w.Log != nil {
		cmd.Stderr = w.Log
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	w.exited = make(chan struct{})
	go func() { w.waitErr = cmd.Wait(); close(w.exited) }()
	go w.read(stdout)
	select {
	case <-w.ready:
		return nil
	case <-w.exited:
		return fmt.Errorf("exited while starting: %v", w.waitErr)
	case <-ctx.Done():
		w.Kill()
		return ctx.Err()
	}
}

func (w *Worker) read(stdout io.Reader) {
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 1<<20), 1<<28)
	readyOnce := sync.Once{}
	for sc.Scan() {
		line := append([]byte(nil), sc.Bytes()...)
		var head struct {
			ID    *int64 `json:"id"`
			Ready bool   `json:"ready"`
		}
		if json.Unmarshal(line, &head) != nil {
			continue
		}
		if head.Ready {
			readyOnce.Do(func() { close(w.ready) })
			continue
		}
		if head.ID == nil {
			continue
		}
		w.mu.Lock()
		ch := w.waiting[*head.ID]
		delete(w.waiting, *head.ID)
		w.mu.Unlock()
		if ch != nil {
			ch <- line
		}
	}
}

var ErrWorkerGone = errors.New("worker exited")

// Ask sends one request (a json object; the id is added) and waits for
// its answer line.
func (w *Worker) Ask(ctx context.Context, request []byte) ([]byte, error) {
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(request, &obj); err != nil {
		return nil, err
	}
	id := w.next.Add(1)
	obj["id"], _ = json.Marshal(id)
	line, _ := json.Marshal(obj)
	ch := make(chan []byte, 1)
	w.mu.Lock()
	w.waiting[id] = ch
	_, err := w.stdin.Write(append(line, '\n'))
	w.mu.Unlock()
	if err != nil {
		return nil, err
	}
	timeout := w.Timeout
	if timeout == 0 {
		timeout = 10 * time.Minute
	}
	select {
	case reply := <-ch:
		return reply, nil
	case <-w.exited:
		return nil, ErrWorkerGone
	case <-time.After(timeout):
		w.mu.Lock()
		delete(w.waiting, id)
		w.mu.Unlock()
		return nil, fmt.Errorf("no answer after %s", timeout)
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (w *Worker) Stop(grace time.Duration) error {
	if w.stdin != nil {
		w.stdin.Close() // a worker exits cleanly at end of input
	}
	return w.process.Stop(grace)
}
