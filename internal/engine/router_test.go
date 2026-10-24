package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fake writes an executable script named name into dir and returns its path.
func fake(t *testing.T, dir, name, body string) string {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

func def(t *testing.T, toml string) *Def {
	t.Helper()
	d, err := parse([]byte(toml), "test")
	if err != nil {
		t.Fatal(err)
	}
	return d
}

func setup(t *testing.T, engines map[string]string, order ...string) (*Router, string) {
	t.Helper()
	bin := t.TempDir()
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	defs := map[string]*Def{}
	for name, script := range engines {
		fake(t, bin, name, script)
		defs[name] = def(t, fmt.Sprintf(`
name = %q
run = [%q, "{prompt}"]
run_limited = [%q, "--limited", "{prompt}"]
exhausted = ['(?i)usage limit reached(\|(?P<reset>\d{10}))?']
cooldown = "30m"
`, name, name, name))
	}
	state := filepath.Join(t.TempDir(), "engines.json")
	r, err := NewRouter(order, defs, state, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	return r, state
}

func TestFirstUsableEngineAnswers(t *testing.T) {
	r, _ := setup(t, map[string]string{
		"a": `echo "answer from a: $1"`,
		"b": `echo "answer from b"`,
	}, "a", "b")
	res, err := r.Run(context.Background(), Task{ID: "t1", Prompt: "hello", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Engine != "a" || res.Output != "answer from a: hello" {
		t.Fatalf("got %s %q", res.Engine, res.Output)
	}
}

func TestFallsBackWhenOutOfUsage(t *testing.T) {
	r, state := setup(t, map[string]string{
		"a": `echo "Claude AI usage limit reached|4102444800"; exit 1`,
		"b": `echo "b did it"`,
	}, "a", "b")
	res, err := r.Run(context.Background(), Task{ID: "t2", Prompt: "work", Dir: t.TempDir()})
	if err != nil {
		t.Fatal(err)
	}
	if res.Engine != "b" || res.Attempts[0].Outcome != "exhausted" {
		t.Fatalf("got engine %s attempts %+v", res.Engine, res.Attempts)
	}
	// The reset time in the message (2100-01-01) is used, and it survives a restart.
	r2, err := NewRouter(r.Order, r.Defs, state, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	c, ok := r2.cooling("a")
	if !ok || c.Until.Year() != 2100 {
		t.Fatalf("cooldown not persisted with reset time: %+v %v", c, ok)
	}
	// While cooling, a is skipped without being run.
	res, err = r2.Run(context.Background(), Task{ID: "t3", Prompt: "again", Dir: t.TempDir()})
	if err != nil || res.Engine != "b" || res.Attempts[0].Duration != 0 {
		t.Fatalf("expected skip of cooling engine, got %+v %v", res, err)
	}
}

func TestNextEngineIsToldAboutPartialWork(t *testing.T) {
	r, _ := setup(t, map[string]string{
		"a": `echo "usage limit reached"; exit 1`,
		"b": `echo "$1"`,
	}, "a", "b")
	res, err := r.Run(context.Background(), Task{ID: "t", Prompt: "fix the bug", Dir: "/tmp"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(res.Output, "a started this task and ran out of usage") || !strings.HasSuffix(res.Output, "fix the bug") {
		t.Fatalf("handoff note missing: %q", res.Output)
	}
}

func TestRealFailureIsNotRetriedElsewhere(t *testing.T) {
	r, _ := setup(t, map[string]string{
		"a": `echo "syntax error in your code"; exit 2`,
		"b": `echo "should not run"`,
	}, "a", "b")
	res, err := r.Run(context.Background(), Task{ID: "t", Prompt: "x", Dir: t.TempDir()})
	if err == nil || res.Engine != "a" || len(res.Attempts) != 1 {
		t.Fatalf("want failure on a only, got %+v %v", res, err)
	}
}

func TestLongAnswerMentioningLimitsIsNotExhaustion(t *testing.T) {
	long := strings.Repeat("Rate limiting explained. ", 30) + "A usage limit reached message means..."
	r, _ := setup(t, map[string]string{"a": "echo '" + long + "'"}, "a")
	res, err := r.Run(context.Background(), Task{ID: "t", Prompt: "explain", Dir: t.TempDir()})
	if err != nil || res.Engine != "a" {
		t.Fatalf("a real answer was treated as exhaustion: %+v %v", res, err)
	}
}

func TestNotSignedInIsSkipped(t *testing.T) {
	r, _ := setup(t, map[string]string{"a": `echo a`, "b": `echo b`}, "a", "b")
	bin := filepath.Dir(must(t, r.Defs["a"].Detect))
	r.Defs["a"].Ready = []string{fake(t, bin, "a-auth", `echo "Not logged in"; exit 1`)}
	res, err := r.Run(context.Background(), Task{ID: "t", Prompt: "x", Dir: t.TempDir()})
	if err != nil || res.Engine != "b" || res.Attempts[0].Outcome != "unavailable" {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestLimitedTasksUseLimitedCommand(t *testing.T) {
	r, _ := setup(t, map[string]string{"a": `echo "args: $*"`}, "a")
	res, err := r.Run(context.Background(), Task{ID: "t", Prompt: "read only please", Dir: t.TempDir(), Limited: true})
	if err != nil || res.Output != "args: --limited read only please" {
		t.Fatalf("got %+v %v", res, err)
	}
	r.Defs["a"].RunLimited = nil
	if _, err := r.Run(context.Background(), Task{ID: "t", Prompt: "x", Dir: t.TempDir(), Limited: true}); !errors.Is(err, ErrNoEngine) {
		t.Fatalf("engine without run_limited took a team task: %v", err)
	}
}

func TestAllExhaustedReportsEveryAttempt(t *testing.T) {
	r, _ := setup(t, map[string]string{
		"a": `echo "usage limit reached"; exit 1`,
		"b": `echo "usage limit reached"; exit 1`,
	}, "a", "b")
	_, err := r.Run(context.Background(), Task{ID: "t", Prompt: "x", Dir: t.TempDir()})
	if !errors.Is(err, ErrNoEngine) || !strings.Contains(err.Error(), "a exhausted") || !strings.Contains(err.Error(), "b exhausted") {
		t.Fatalf("got %v", err)
	}
}

func TestPreferredEngineGoesFirst(t *testing.T) {
	r, _ := setup(t, map[string]string{"a": `echo a`, "b": `echo b`}, "a", "b")
	res, err := r.Run(context.Background(), Task{ID: "t", Prompt: "x", Dir: t.TempDir(), Prefer: "b"})
	if err != nil || res.Engine != "b" {
		t.Fatalf("got %+v %v", res, err)
	}
}

func TestTimeoutKillsTheEngine(t *testing.T) {
	r, _ := setup(t, map[string]string{"a": `sleep 30; echo late`}, "a")
	r.Timeout = 300 * time.Millisecond
	start := time.Now()
	_, err := r.Run(context.Background(), Task{ID: "t", Prompt: "x", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 5*time.Second {
		t.Fatalf("got %v after %s", err, time.Since(start))
	}
}

func TestBuiltinsParse(t *testing.T) {
	defs, err := Load(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"claude", "codex", "gemini"} {
		if defs[n] == nil || len(defs[n].exhausted) == 0 {
			t.Fatalf("built-in %s missing or without exhaustion patterns", n)
		}
	}
	// Claude's limit message with a reset time.
	ex, until := defs["claude"].exhaustion("Claude AI usage limit reached|4102444800", true)
	if !ex || until.Year() != 2100 {
		t.Fatalf("claude exhaustion: %v %v", ex, until)
	}
}

func TestUserDefinitionOverridesBuiltin(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "codex.toml"), []byte(`name = "codex"
run = ["my-codex", "{prompt}"]`), 0o644)
	os.WriteFile(filepath.Join(dir, "opencode.toml"), []byte(`name = "opencode"
run = ["opencode", "run", "{prompt}"]`), 0o644)
	defs, err := Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if defs["codex"].Run[0] != "my-codex" || defs["opencode"] == nil || defs["claude"] == nil {
		t.Fatalf("override/add failed: %+v", defs)
	}
}

func must(t *testing.T, name string) string {
	t.Helper()
	p, err := exec.LookPath(name)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestCancelIsNotAFailure(t *testing.T) {
	r, _ := setup(t, map[string]string{"a": `sleep 30`, "b": `echo b`}, "a", "b")
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(200*time.Millisecond, cancel)
	res, err := r.Run(ctx, Task{ID: "t", Prompt: "x", Dir: t.TempDir()})
	if !errors.Is(err, ErrCancelled) || len(res.Attempts) != 1 || res.Attempts[0].Outcome != "cancelled" {
		t.Fatalf("want a cancelled attempt on a only, got %+v %v", res, err)
	}
}

func TestSilentFailureSaysWhy(t *testing.T) {
	r, _ := setup(t, map[string]string{"a": `exit 3`}, "a")
	_, err := r.Run(context.Background(), Task{ID: "t", Prompt: "x", Dir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "exit status 3, no output") {
		t.Fatalf("got %v", err)
	}
}
