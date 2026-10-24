package engine

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Router picks an engine for each task and falls back down its order when one is out of usage.
type Router struct {
	Order   []string
	Defs    map[string]*Def
	State   string        // file holding cooldowns, so they survive restarts
	Timeout time.Duration // per engine run

	mu    sync.Mutex
	cool  map[string]Cooldown
	ready map[string]readyCache
}

// Cooldown records that an engine ran out of usage.
type Cooldown struct {
	Until  time.Time `json:"until"`
	Reason string    `json:"reason"`
}

type readyCache struct {
	ok  bool
	why string
	at  time.Time
}

// Task is one unit of work for the router.
type Task struct {
	ID      string
	Prompt  string
	Dir     string
	Limited bool   // team request: only engines with run_limited, run read-only
	Prefer  string // try this engine first if it is usable
	Hops    int    // agent-to-agent depth, passed on so nested asks keep counting
}

// Attempt is one engine try within a task.
type Attempt struct {
	Engine   string        `json:"engine"`
	Outcome  string        `json:"outcome"` // ok | exhausted | unavailable | failed
	Detail   string        `json:"detail,omitempty"`
	Duration time.Duration `json:"duration"`
}

// Result is the router's answer to a task.
type Result struct {
	Engine   string    `json:"engine"`
	Output   string    `json:"output"`
	Attempts []Attempt `json:"attempts"`
}

// Status describes one engine for `agent-collaborator engines`.
type Status struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Installed   bool      `json:"installed"`
	Ready       bool      `json:"ready"`
	Detail      string    `json:"detail,omitempty"`
	CoolUntil   time.Time `json:"cool_until,omitempty"`
	Limited     bool      `json:"limited"` // can take team tasks
	Source      string    `json:"source"`
}

// ErrCancelled means the task was stopped from outside (Ctrl-C, daemon shutdown), not that the
// engine failed: the router does not try another engine.
var ErrCancelled = errors.New("cancelled before it finished")

// ErrNoEngine means every engine in the order was unavailable or out of usage.
var ErrNoEngine = errors.New("no usable engine")

// NewRouter builds a router and loads persisted cooldowns.
func NewRouter(order []string, defs map[string]*Def, stateFile string, timeout time.Duration) (*Router, error) {
	for _, n := range order {
		if defs[n] == nil {
			return nil, fmt.Errorf("engine %q is in the order but not defined", n)
		}
	}
	r := &Router{Order: order, Defs: defs, State: stateFile, Timeout: timeout,
		cool: map[string]Cooldown{}, ready: map[string]readyCache{}}
	if b, err := os.ReadFile(stateFile); err == nil {
		_ = json.Unmarshal(b, &r.cool)
	}
	return r, nil
}

// Statuses reports every engine in the order.
func (r *Router) Statuses(ctx context.Context) []Status {
	var out []Status
	for _, n := range r.Order {
		d := r.Defs[n]
		s := Status{Name: n, Description: d.Description, Limited: len(d.RunLimited) > 0, Source: d.source}
		_, err := exec.LookPath(d.Detect)
		s.Installed = err == nil
		if s.Installed {
			s.Ready, s.Detail = r.isReady(ctx, d, true)
		} else {
			s.Detail = d.Detect + " not on PATH"
		}
		if c, ok := r.cooling(n); ok {
			s.CoolUntil, s.Detail = c.Until, "out of usage: "+c.Reason
		}
		out = append(out, s)
	}
	return out
}

// ClearCooldown puts an engine back into rotation (e.g. after topping up credit).
func (r *Router) ClearCooldown(name string) {
	r.mu.Lock()
	delete(r.cool, name)
	r.saveLocked()
	r.mu.Unlock()
}

// Run executes a task on the first usable engine, moving down the order when an engine is not
// signed in or runs out of usage. A task that fails for any other reason is not retried
// elsewhere: that would double the cost of a genuinely broken task.
func (r *Router) Run(ctx context.Context, t Task) (*Result, error) {
	res := &Result{}
	var handoff string
	for _, name := range r.order(t.Prefer) {
		d := r.Defs[name]
		start := time.Now()
		att := Attempt{Engine: name}
		tmpl := d.Run
		if t.Limited {
			tmpl = d.RunLimited
		}
		if att.Outcome, att.Detail = r.precheck(ctx, d, tmpl); att.Outcome != "" {
			res.Attempts = append(res.Attempts, att)
			continue
		}

		out, failed, err := r.exec(ctx, d, tmpl, handoff+t.Prompt, t)
		att.Duration = time.Since(start).Round(time.Second)
		if errors.Is(err, ErrCancelled) {
			att.Outcome, att.Detail = "cancelled", err.Error()
			res.Attempts = append(res.Attempts, att)
			return res, fmt.Errorf("%s: %w", name, err)
		}
		if ex, until := d.exhaustion(out, failed); ex {
			reason := firstLine(lastMatch(d, out))
			r.setCooldown(name, until, reason)
			att.Outcome, att.Detail = "exhausted", reason
			res.Attempts = append(res.Attempts, att)
			handoff = fmt.Sprintf("Note: %s started this task and ran out of usage partway. Files in %s may already "+
				"hold partial changes: check (e.g. git status / git diff) before redoing work.\n\n", name, t.Dir)
			continue
		}
		if err != nil || failed {
			att.Outcome, att.Detail = "failed", errDetail(err, out)
			res.Attempts = append(res.Attempts, att)
			res.Engine, res.Output = name, out
			return res, fmt.Errorf("%s failed: %s", name, att.Detail)
		}
		att.Outcome = "ok"
		res.Attempts = append(res.Attempts, att)
		res.Engine, res.Output = name, strings.TrimSpace(out)
		return res, nil
	}
	return res, fmt.Errorf("%w: %s", ErrNoEngine, summarize(res.Attempts))
}

// precheck returns a non-empty outcome when the engine can't take the task right now.
func (r *Router) precheck(ctx context.Context, d *Def, tmpl []string) (outcome, detail string) {
	if len(tmpl) == 0 {
		return "unavailable", "no limited mode for team requests"
	}
	if c, ok := r.cooling(d.Name); ok {
		return "exhausted", fmt.Sprintf("%s (until %s)", c.Reason, c.Until.Format("Jan 2 15:04"))
	}
	if _, err := exec.LookPath(d.Detect); err != nil {
		return "unavailable", d.Detect + " not installed"
	}
	if ok, why := r.isReady(ctx, d, false); !ok {
		return "unavailable", why
	}
	return "", ""
}

func (r *Router) order(prefer string) []string {
	if prefer == "" || r.Defs[prefer] == nil {
		return r.Order
	}
	o := []string{prefer}
	for _, n := range r.Order {
		if n != prefer {
			o = append(o, n)
		}
	}
	return o
}

// exec runs one engine command. failed is true for a non-zero exit or a timeout.
func (r *Router) exec(ctx context.Context, d *Def, tmpl []string, prompt string, t Task) (out string, failed bool, err error) {
	tmp, err := os.MkdirTemp("", "agent-collaborator-task-")
	if err != nil {
		return "", true, err
	}
	defer os.RemoveAll(tmp)
	outFile := filepath.Join(tmp, "answer.md")
	args := command(tmpl, prompt, t.Dir, outFile)

	if r.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Timeout)
		defer cancel()
	}
	cmd := exec.CommandContext(ctx, args[0], args[1:]...)
	cmd.Dir = t.Dir
	cmd.Env = append(os.Environ(), "AC_TASK="+t.ID, fmt.Sprintf("AC_HOPS=%d", t.Hops))
	cmd.Stdin = nil // engines that read stdin when it isn't a terminal must not hang
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM) }
	cmd.WaitDelay = 10 * time.Second
	var buf limitedBuffer
	buf.max = 1 << 20
	cmd.Stdout, cmd.Stderr = &buf, &buf
	runErr := cmd.Run()
	logs := buf.String()

	answer := logs
	if d.Output == "file" {
		if b, ferr := os.ReadFile(outFile); ferr == nil && len(bytes.TrimSpace(b)) > 0 {
			answer = string(b)
		}
	}
	switch {
	case errors.Is(ctx.Err(), context.DeadlineExceeded):
		return logs, true, fmt.Errorf("timed out after %s", r.Timeout)
	case ctx.Err() != nil:
		return logs, true, ErrCancelled
	case runErr != nil:
		if strings.TrimSpace(logs) == "" {
			return logs, true, fmt.Errorf("%v, no output", runErr) // e.g. "exit status 1, no output"
		}
		return logs, true, nil // logs carry the reason (and any exhaustion message)
	}
	return answer, false, nil
}

// isReady runs the engine's sign-in check, cached for a minute (fresh=true bypasses the cache).
func (r *Router) isReady(ctx context.Context, d *Def, fresh bool) (bool, string) {
	if len(d.Ready) == 0 {
		return true, ""
	}
	r.mu.Lock()
	c, ok := r.ready[d.Name]
	r.mu.Unlock()
	if ok && !fresh && time.Since(c.at) < time.Minute {
		return c.ok, c.why
	}
	cctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(cctx, d.Ready[0], d.Ready[1:]...)
	cmd.Stdin = nil
	b, err := cmd.CombinedOutput()
	okNow, why := err == nil, ""
	if okNow && d.ready != nil && !d.ready.Match(b) {
		okNow = false
	}
	if !okNow {
		why = "check: " + strings.Join(d.Ready, " ")
	}
	r.mu.Lock()
	r.ready[d.Name] = readyCache{okNow, why, time.Now()}
	r.mu.Unlock()
	return okNow, why
}

func (r *Router) cooling(name string) (Cooldown, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.cool[name]
	if ok && time.Now().After(c.Until) {
		delete(r.cool, name)
		r.saveLocked()
		return Cooldown{}, false
	}
	return c, ok
}

func (r *Router) setCooldown(name string, until time.Time, reason string) {
	r.mu.Lock()
	r.cool[name] = Cooldown{Until: until, Reason: reason}
	r.saveLocked()
	r.mu.Unlock()
}

func (r *Router) saveLocked() {
	if r.State == "" {
		return
	}
	b, _ := json.MarshalIndent(r.cool, "", "  ")
	tmp := r.State + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		_ = os.Rename(tmp, r.State)
	}
}

func lastMatch(d *Def, out string) string {
	for _, re := range d.exhausted {
		if loc := re.FindAllStringIndex(out, -1); loc != nil {
			l := loc[len(loc)-1]
			start := strings.LastIndex(out[:l[0]], "\n") + 1
			return out[start:]
		}
	}
	return "usage limit"
}

func firstLine(s string) string {
	s, _, _ = strings.Cut(strings.TrimSpace(s), "\n")
	if len(s) > 160 {
		s = s[:160] + "…"
	}
	return s
}

var ansi = regexp.MustCompile(`\x1b\[[0-9;]*[a-zA-Z]`)

func errDetail(err error, out string) string {
	if err != nil {
		return err.Error()
	}
	lines := strings.Split(strings.TrimSpace(ansi.ReplaceAllString(out, "")), "\n")
	if n := len(lines); n > 3 {
		lines = lines[n-3:]
	}
	return firstLine(strings.Join(lines, " | "))
}

func summarize(atts []Attempt) string {
	if len(atts) == 0 {
		return "no engines configured"
	}
	var parts []string
	for _, a := range atts {
		parts = append(parts, fmt.Sprintf("%s %s (%s)", a.Engine, a.Outcome, a.Detail))
	}
	return strings.Join(parts, "; ")
}

// limitedBuffer keeps the last max bytes written to it.
type limitedBuffer struct {
	mu  sync.Mutex
	b   []byte
	max int
}

func (l *limitedBuffer) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.b = append(l.b, p...)
	if len(l.b) > l.max {
		l.b = l.b[len(l.b)-l.max:]
	}
	return len(p), nil
}

func (l *limitedBuffer) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return string(l.b)
}
