package node

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TheAdaply/agent-collaborator/internal/config"
	"github.com/TheAdaply/agent-collaborator/internal/proto"
	"github.com/TheAdaply/agent-collaborator/internal/store"
)

// testNode builds a node named "here" in local mode with a fake engine "e" that echoes its prompt.
func testNode(t *testing.T) *Node {
	t.Helper()
	home := t.TempDir()
	t.Setenv("AC_HOME", home)
	bin := t.TempDir()
	os.WriteFile(filepath.Join(bin, "fake-e"), []byte("#!/bin/sh\necho \"done: $(echo \"$1\" | tail -1)\"\n"), 0o755)
	t.Setenv("PATH", bin+":/usr/bin:/bin")
	os.MkdirAll(filepath.Join(home, "engines"), 0o700)
	os.WriteFile(filepath.Join(home, "engines", "e.toml"), []byte(`name = "e"
run = ["fake-e", "{prompt}"]
run_limited = ["fake-e", "{prompt}"]`), 0o600)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte(`name = "here"
network = "local"
engines = ["e"]
[trust]
team = ["mate@github"]
rate_per_minute = 3`), 0o600)
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	n, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	n.owner = "me@github"
	return n
}

func msg(kind proto.Kind, to string, trust proto.Trust, login string) *proto.Message {
	return &proto.Message{ID: proto.NewID(), Kind: kind, To: proto.Address(to), From: "there/agent",
		Body: "please do x", Hops: 1, Sent: time.Now(), Trust: trust,
		Sender: &proto.Identity{Login: login, Machine: "there"}}
}

func TestTrustLevels(t *testing.T) {
	n := testNode(t)
	for login, want := range map[string]proto.Trust{
		"me@github": proto.Owner, "ME@GitHub": proto.Owner, "mate@github": proto.Team, "stranger@github": proto.Unknown,
	} {
		if got := n.trustOf(login); got != want {
			t.Errorf("trustOf(%s) = %s, want %s", login, got, want)
		}
	}
}

func TestTeamCannotReachUnsharedSession(t *testing.T) {
	n := testNode(t)
	n.st.PutSession(store.Session{Name: "private", Deliver: "inbox"})
	n.st.PutSession(store.Session{Name: "shared", Deliver: "inbox", Team: true})
	if r := n.accept(context.Background(), msg(proto.Tell, "here/private", proto.Team, "mate@github")); r.State != "refused" {
		t.Fatalf("team reached a private session: %+v", r)
	}
	if r := n.accept(context.Background(), msg(proto.Tell, "here/shared", proto.Team, "mate@github")); r.State != "delivered" {
		t.Fatalf("team could not reach a shared session: %+v", r)
	}
	if r := n.accept(context.Background(), msg(proto.Tell, "here/private", proto.Owner, "me@github")); r.State != "delivered" {
		t.Fatalf("owner could not reach own session: %+v", r)
	}
}

func TestNoForwardingAndUnknownSession(t *testing.T) {
	n := testNode(t)
	if r := n.accept(context.Background(), msg(proto.Tell, "elsewhere", proto.Owner, "me@github")); r.State != "refused" {
		t.Fatalf("relayed a message for another machine: %+v", r)
	}
	if r := n.accept(context.Background(), msg(proto.Tell, "here/nobody", proto.Owner, "me@github")); r.State != "refused" {
		t.Fatalf("accepted a message for a missing session: %+v", r)
	}
}

func TestReplyOnlyFromTheAskedMachine(t *testing.T) {
	n := testNode(t)
	n.st.Asked("ask-1", "there")
	forged := &proto.Message{ID: proto.NewID(), Kind: proto.Reply, ReplyTo: "ask-1", To: "here", From: "intruder", Body: "fake"}
	if r := n.accept(context.Background(), forged); r.State != "refused" {
		t.Fatalf("accepted a reply from a machine that was never asked: %+v", r)
	}
	if _, ok := n.st.Reply("ask-1"); ok {
		t.Fatal("forged reply was stored")
	}
	real := &proto.Message{ID: proto.NewID(), Kind: proto.Reply, ReplyTo: "ask-1", To: "here", From: "there/agent", Body: "real"}
	if r := n.accept(context.Background(), real); r.State != "delivered" {
		t.Fatalf("real reply refused: %+v", r)
	}
	if m, ok := n.st.Reply("ask-1"); !ok || m.Body != "real" {
		t.Fatalf("reply not stored: %+v", m)
	}
}

func TestHopLimitStopsAgentLoops(t *testing.T) {
	n := testNode(t)
	m := msg(proto.Ask, "here", proto.Owner, "me@github")
	m.Hops = proto.MaxHops + 1
	if r := n.accept(context.Background(), m); r.State != "refused" || !strings.Contains(r.Detail, "loop") {
		t.Fatalf("hop limit not enforced: %+v", r)
	}
}

func TestRouterAnswersAskAndTeamGetsLimitedMode(t *testing.T) {
	n := testNode(t)
	m := msg(proto.Ask, "here", proto.Team, "mate@github")
	m.From = "here/requester" // reply comes back to this machine, so it lands in the reply store
	if r := n.accept(context.Background(), m); r.State != "routing" {
		t.Fatalf("not routed: %+v", r)
	}
	n.wg.Wait()
	rep, ok := n.st.Reply(m.ID)
	if !ok {
		t.Fatal("no reply from router")
	}
	if !strings.Contains(rep.Body, "done: please do x") || !strings.Contains(rep.Body, "answered by e on here") {
		t.Fatalf("unexpected reply: %q", rep.Body)
	}
	if !strings.Contains(routerPrompt(m), "read-only mode") {
		t.Fatal("team task prompt does not say read-only")
	}
}

func TestEachAskIsAnsweredOnce(t *testing.T) {
	n := testNode(t)
	n.st.PutSession(store.Session{Name: "worker", Deliver: "inbox"})
	m := msg(proto.Ask, "here/worker", proto.Owner, "me@github")
	m.From = "here/boss"
	n.accept(context.Background(), m)
	if _, err := n.reply(context.Background(), m.ID, "first"); err != nil {
		t.Fatal(err)
	}
	if _, err := n.reply(context.Background(), m.ID, "second"); err == nil {
		t.Fatal("answered the same ask twice")
	}
}

func TestRateLimit(t *testing.T) {
	l := newLimiter(3)
	for i := 0; i < 3; i++ {
		if !l.allow("a") {
			t.Fatal("blocked under the limit")
		}
	}
	if l.allow("a") {
		t.Fatal("not blocked over the limit")
	}
	if !l.allow("b") {
		t.Fatal("limit leaked across senders")
	}
}

func TestOwnerReachesDetectedLiveSession(t *testing.T) {
	n := testNode(t)
	// Simulate a live Claude Code session that never registered with Agent Collaborator.
	cdir := t.TempDir()
	setClaudeDir(t, cdir)
	// target() only reads the session registry; it never dials the socket, so no listener is needed
	// (creating one here broke on macOS, where the long temp path exceeds the unix-socket path limit).
	writeClaudeSession(t, cdir, "my-ide", "sess-xyz", filepath.Join(cdir, "s.sock"))

	// Owner may reach it by name even though it's unregistered.
	s, err := n.target(msg(proto.Tell, "here/my-ide", proto.Owner, "me@github"))
	if err != nil || s == nil || !s.Detected || s.Deliver != "claude:sess-xyz" {
		t.Fatalf("owner could not reach a detected session: %+v %v", s, err)
	}
	// Team may NOT reach an unregistered session.
	if _, err := n.target(msg(proto.Tell, "here/my-ide", proto.Team, "mate@github")); err == nil {
		t.Fatal("team reached an unregistered live session")
	}
}

func TestWhoListsDetectedSessionsForOwnerOnly(t *testing.T) {
	n := testNode(t)
	cdir := t.TempDir()
	setClaudeDir(t, cdir)
	writeClaudeSession(t, cdir, "ide-a", "id-a", filepath.Join(cdir, "a.sock"))
	owner := n.who(context.Background(), proto.Owner)
	var found bool
	for _, s := range owner.Sessions {
		if s.Name == "ide-a" && s.Detected {
			found = true
		}
	}
	if !found {
		t.Fatalf("owner who did not list the detected session: %+v", owner.Sessions)
	}
	team := n.who(context.Background(), proto.Team)
	for _, s := range team.Sessions {
		if s.Name == "ide-a" {
			t.Fatal("team who leaked a detected session")
		}
	}
}

func TestNicknameResolution(t *testing.T) {
	n := testNode(t)
	n.st.PutSession(store.Session{Name: "linux-guy", Deliver: "inbox"})
	// "mac guy" style spaces and case normalize to the session name.
	got, err := n.resolve(context.Background(), "Linux Guy")
	if err != nil || got != "here/linux-guy" {
		t.Fatalf("nickname not resolved: %q %v", got, err)
	}
	// A bare machine name stays a router address.
	if got, _ := n.resolve(context.Background(), "here"); got != "here" {
		t.Fatalf("machine name mis-resolved: %q", got)
	}
	// An explicit machine/session is left untouched.
	if got, _ := n.resolve(context.Background(), "here/linux-guy"); got != "here/linux-guy" {
		t.Fatalf("explicit address changed: %q", got)
	}
	// Unknown nickname is left as-is (delivery will give a clear error).
	if got, _ := n.resolve(context.Background(), "ghost"); got != "ghost" {
		t.Fatalf("unknown nick changed: %q", got)
	}
}
