// Package node is the Agent Collaborator daemon: it receives messages from peers over the tailnet, decides
// how much to trust each sender, and delivers to a live agent session or to the engine router.
package node

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/TheAdaply/agent-collaborator/internal/command"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/TheAdaply/agent-collaborator/internal/config"
	"github.com/TheAdaply/agent-collaborator/internal/deliver"
	"github.com/TheAdaply/agent-collaborator/internal/engine"
	"github.com/TheAdaply/agent-collaborator/internal/proto"
	"github.com/TheAdaply/agent-collaborator/internal/store"
)

// Version is set at build time.
var Version = "dev"

// Node is one machine's daemon.
type Node struct {
	cfg    *config.Config
	tr     Transport
	st     *store.Store
	router *engine.Router
	owner  string
	peers  *http.Client
	logf   func(string, ...any)

	tasks chan struct{} // bounds concurrent router tasks
	rate  *limiter
	wg    sync.WaitGroup
}

// New builds a node from its config.
func New(cfg *config.Config) (*Node, error) {
	logger := log.New(os.Stderr, "Agent Collaborator: ", log.LstdFlags)
	st, err := store.Open(cfg.StateDir)
	if err != nil {
		return nil, err
	}
	defs, err := engine.Load(cfg.EnginesDir())
	if err != nil {
		return nil, err
	}
	rt, err := engine.NewRouter(cfg.Engines, defs, filepath.Join(cfg.StateDir, "engines.json"), cfg.TaskTimeout.Duration)
	if err != nil {
		return nil, err
	}
	tr, err := newTransport(cfg, logger.Printf)
	if err != nil {
		return nil, err
	}
	n := &Node{cfg: cfg, tr: tr, st: st, router: rt, logf: logger.Printf,
		tasks: make(chan struct{}, 2), rate: newLimiter(cfg.Trust.RatePerMinute)}
	n.peers = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, addr string) (net.Conn, error) {
				host, _, _ := net.SplitHostPort(addr)
				return tr.Dial(ctx, host)
			},
			MaxIdleConnsPerHost: 2,
		},
	}
	if err := n.interruptTasks(); err != nil {
		return nil, err
	}
	return n, nil
}

// Run serves until ctx is cancelled.
func (n *Node) Run(ctx context.Context) error {
	ctlLn, err := listenSocket(n.cfg.Socket())
	if err != nil {
		return err
	}
	ctl := &http.Server{Handler: n.controlAPI(), ReadHeaderTimeout: 10 * time.Second}
	go ctl.Serve(ctlLn)
	defer ctl.Close()
	n.logf("control socket %s", n.cfg.Socket())

	n.logf("joining the network (%s) as %s", n.cfg.Network, n.cfg.Name)
	peerLn, err := n.tr.Listen()
	if err != nil {
		return err
	}
	defer n.tr.Close()
	if name, err := n.tr.Name(ctx); err == nil && name != "" && name != n.cfg.Name {
		n.logf("using the tailnet name %q for this machine (config says %q)", name, n.cfg.Name)
		n.cfg.Name = name
	}
	if n.owner = n.cfg.Owner; n.owner == "" {
		if n.owner, err = n.tr.Login(ctx); err != nil {
			return fmt.Errorf("finding this node's login: %w", err)
		}
	}
	peer := &http.Server{Handler: n.peerAPI(), ReadHeaderTimeout: 10 * time.Second}
	go peer.Serve(peerLn)
	defer peer.Close()
	n.logf("Agent Collaborator %s up: machine %s, owner %s, engines %v", Version, n.cfg.Name, n.owner, n.cfg.Engines)

	go n.retryOutbox(ctx)
	<-ctx.Done()
	n.wg.Wait()
	return nil
}

func listenSocket(path string) (net.Listener, error) {
	if c, err := net.Dial("unix", path); err == nil {
		c.Close()
		return nil, fmt.Errorf("another Agent Collaborator daemon is running (%s)", path)
	}
	_ = os.Remove(path)
	old := umask(0o077)
	ln, err := net.Listen("unix", path)
	umask(old)
	return ln, err
}

// --- peer API: what other machines can call ---

func (n *Node) peerAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", n.handleFleet)
	mux.HandleFunc("GET /fleet", n.handleFleet)
	mux.HandleFunc("POST /v1/msg", n.handlePeerMsg)
	mux.HandleFunc("GET /v1/who", n.handlePeerWho)
	return mux
}

type msgResult struct {
	ID     string `json:"id"`
	State  string `json:"state"` // delivered | routing | queued | refused | error
	Detail string `json:"detail,omitempty"`
}

func (n *Node) identify(w http.ResponseWriter, r *http.Request) (*proto.Identity, proto.Trust, bool) {
	id, err := n.tr.Identify(r.Context(), r)
	if err != nil {
		http.Error(w, "unidentified: "+err.Error(), http.StatusUnauthorized)
		return nil, "", false
	}
	trust := n.trustOf(id.Login)
	if trust == proto.Unknown {
		n.st.Audit(store.AuditEntry{Dir: "in", From: proto.Address(id.Machine), Trust: trust, Action: "refused", Detail: id.Login + " is not owner or team"})
		http.Error(w, "refused: "+id.Login+" is not allowed on this machine", http.StatusForbidden)
		return nil, "", false
	}
	return id, trust, true
}

func (n *Node) trustOf(login string) proto.Trust {
	if strings.EqualFold(login, n.owner) {
		return proto.Owner
	}
	for _, t := range n.cfg.Trust.Team {
		if strings.EqualFold(login, t) {
			return proto.Team
		}
	}
	return proto.Unknown
}

func (n *Node) handlePeerMsg(w http.ResponseWriter, r *http.Request) {
	id, trust, ok := n.identify(w, r)
	if !ok {
		return
	}
	if !n.rate.allow(id.Login + "/" + id.Machine) {
		http.Error(w, "rate limit: too many messages, slow down", http.StatusTooManyRequests)
		return
	}
	var m proto.Message
	if err := json.NewDecoder(io.LimitReader(r.Body, proto.MaxBody+64<<10)).Decode(&m); err != nil {
		http.Error(w, "bad message: "+err.Error(), http.StatusBadRequest)
		return
	}
	// The network says which machine this is; the machine speaks for its own sessions.
	m.From = proto.Address(id.Machine)
	if claimed := proto.Address(r.Header.Get("X-Agent-Collaborator-From")).Session(); claimed != "" {
		m.From = proto.Address(id.Machine + "/" + claimed)
	}
	m.Sender, m.Trust = id, trust
	writeJSON(w, n.accept(r.Context(), &m))
}

// accept validates and dispatches a message addressed to this machine.
func (n *Node) accept(ctx context.Context, m *proto.Message) msgResult {
	res := msgResult{ID: m.ID}
	fail := func(state, detail string) msgResult {
		res.State, res.Detail = state, detail
		n.st.Audit(store.AuditEntry{Dir: "in", ID: m.ID, Kind: m.Kind, From: m.From, To: m.To, Trust: m.Trust, Action: state, Detail: detail, Body: clip(m.Body)})
		return res
	}
	if err := m.Validate(); err != nil {
		return fail("refused", err.Error())
	}
	if m.To.Machine() != n.cfg.Name {
		return fail("refused", fmt.Sprintf("this is %s, not %s (no forwarding)", n.cfg.Name, m.To.Machine()))
	}

	if m.Kind == proto.Reply {
		asked, ok := n.st.AskedOf(m.ReplyTo)
		if !ok || asked != m.From.Machine() {
			return fail("refused", "reply to an ask this machine did not send to "+m.From.Machine())
		}
		if err := n.st.PutReply(m); err != nil {
			return fail("error", err.Error())
		}
		return fail("delivered", "reply stored")
	}

	sess, err := n.target(m)
	if err != nil {
		return fail("refused", err.Error())
	}
	task, created, err := n.st.CreateTask(store.Task{ID: m.ID, Kind: m.Kind, From: m.From, To: m.To, Body: m.Body, Trust: m.Trust})
	if err != nil {
		return fail("error", err.Error())
	}
	if !created {
		if task.From != m.From || task.To != m.To || task.Kind != m.Kind || task.Trust != m.Trust {
			return fail("refused", "task ID belongs to another message")
		}
		if (task.State == store.Reported || task.State == store.Failed || task.State == store.Interrupted) && m.Kind == proto.Ask {
			body := task.ResultSnippet
			if body == "" {
				body = "Already handled: " + string(task.State)
				if task.Error != "" {
					body += ": " + task.Error
				}
			}
			reply := &proto.Message{ID: proto.NewID(), Kind: proto.Reply, ReplyTo: task.ID, From: task.To, To: task.From, Body: body, Sent: time.Now()}
			if reply.To.Machine() == n.cfg.Name {
				if err := n.st.PutReply(reply); err != nil {
					return fail("error", err.Error())
				}
			} else {
				n.send(ctx, reply)
			}
		}
		return fail("delivered", "duplicate task: "+string(task.State))
	}
	if err := n.st.TransitionTask(m.ID, store.Running, "", "", ""); err != nil {
		return fail("error", err.Error())
	}
	finishDelivery := func(state, detail string) msgResult {
		if state == "error" || m.Kind == proto.Tell {
			next := store.Reported
			problem := ""
			if state == "error" {
				next = store.Failed
				problem = detail
			}
			if err := n.st.TransitionTask(m.ID, next, "", "", problem); err != nil {
				return fail("error", err.Error())
			}
		}
		return fail(state, detail)
	}
	if m.Kind == proto.Ask {
		if err := n.st.PutPending(m); err != nil {
			return finishDelivery("error", err.Error())
		}
	}
	switch {
	case sess == nil || sess.Deliver == "router":
		n.wg.Add(1)
		go n.route(m)
		return fail("routing", "the router is running it on the first usable engine")
	case sess.Deliver == "inbox":
		if err := n.st.PutInbox(sess.Name, m); err != nil {
			return finishDelivery("error", err.Error())
		}
		return finishDelivery("delivered", "in "+sess.Name+"'s inbox")
	case strings.HasPrefix(sess.Deliver, "claude:"):
		ref, _ := deliver.ClaudeRef(sess.Deliver)
		if err := deliver.Claude(m, ref); err != nil {
			// The socket may be gone or the session busy with tools: keep it for the inbox, and also
			// type it into the terminal if we know one, so an idle session still sees it.
			inboxErr := n.st.PutInbox(sess.Name, m)
			if t, perr := deliver.ParseTmux(sess.Fallback); perr == nil {
				if terr := deliver.Tmux(ctx, t, m, filepath.Join(n.cfg.StateDir, "spool")); terr == nil {
					if inboxErr != nil {
						return finishDelivery("error", inboxErr.Error())
					}
					return finishDelivery("delivered", "push failed ("+err.Error()+"); typed into "+sess.Name)
				}
			}
			if inboxErr != nil {
				return finishDelivery("error", inboxErr.Error())
			}
			return finishDelivery("delivered", "push failed ("+err.Error()+"); kept in "+sess.Name+"'s inbox")
		}
		return finishDelivery("delivered", "pushed into Claude session "+sess.Name)
	case strings.HasPrefix(sess.Deliver, "tmux:"):
		t, err := deliver.ParseTmux(sess.Deliver)
		if err == nil {
			err = deliver.Tmux(ctx, t, m, filepath.Join(n.cfg.StateDir, "spool"))
		}
		if err != nil {
			// The session may be gone or busy: keep the message for it rather than lose it.
			if inboxErr := n.st.PutInbox(sess.Name, m); inboxErr != nil {
				return finishDelivery("error", inboxErr.Error())
			}
			return finishDelivery("delivered", "typed delivery failed ("+err.Error()+"); kept in "+sess.Name+"'s inbox")
		}
		return finishDelivery("delivered", "typed into "+sess.Name)
	}
	return finishDelivery("error", "session "+sess.Name+" has unknown delivery "+sess.Deliver)
}

// prune drops sessions whose process or terminal has gone, so who never lists dead ones and messages to
// them fail clearly instead of piling up. Messages already in a pruned session's inbox stay on disk.
func (n *Node) prune(ctx context.Context) {
	removed := n.st.Prune(func(s store.Session) bool {
		if s.PID > 0 {
			if err := syscall.Kill(s.PID, 0); errors.Is(err, syscall.ESRCH) {
				return true
			}
		}
		if t, err := deliver.ParseTmux(s.Deliver); err == nil {
			return !t.Alive(ctx)
		}
		if ref, ok := deliver.ClaudeRef(s.Deliver); ok {
			_, live := deliver.FindClaude(ref)
			return !live // the Claude session's registry file is gone
		}
		return false
	})
	for _, name := range removed {
		n.logf("session %s is gone; removed", name)
	}
}

// target resolves the session a message is for (nil: the router) and enforces team access.
func (n *Node) target(m *proto.Message) (*store.Session, error) {
	name := m.To.Session()
	if name == "" {
		return nil, nil
	}
	n.prune(context.Background())
	sessions, err := n.st.Sessions()
	if err != nil {
		return nil, err
	}
	s, ok := sessions[name]
	if !ok {
		// Owner can reach ANY live Claude session on this machine by name or id, even one that never
		// registered (e.g. an interactive session you started yourself): push a prompt straight into it.
		if m.Trust == proto.Owner {
			if cs, live := deliver.FindClaude(name); live {
				return &store.Session{Name: name, Deliver: "claude:" + cs.SessionID, Agent: "claude", Detected: true}, nil
			}
		}
		return nil, fmt.Errorf(command.Text("no session %q on %s (@COMMAND@ who)"), name, n.cfg.Name)
	}
	if m.Trust == proto.Team && !s.Team {
		return nil, fmt.Errorf("session %q is not shared with teammates", name)
	}
	return &s, nil
}

// route runs a message as a task on the engine router and answers asks with the result.
func (n *Node) route(m *proto.Message) {
	defer n.wg.Done()
	n.tasks <- struct{}{}
	defer func() { <-n.tasks }()

	ctx := context.Background()
	res, err := n.router.Run(ctx, engine.Task{
		ID: m.ID, Prompt: routerPrompt(m), Dir: n.cfg.Workdir,
		Limited: m.Trust != proto.Owner, Prefer: m.Prefer, Hops: m.Hops,
	})
	action, body := "routed", ""
	switch {
	case err != nil && res != nil && res.Output != "":
		action, body = "failed", fmt.Sprintf("FAILED on %s: %v\n\n%s", res.Engine, err, tail(res.Output, 4000))
	case err != nil:
		action, body = "failed", "FAILED: "+err.Error()
	default:
		action, body = "routed:"+res.Engine, res.Output
	}
	if res != nil {
		body += "\n\n-- " + footer(n.cfg.Name, res)
	}
	n.st.Audit(store.AuditEntry{Dir: "in", ID: m.ID, Kind: m.Kind, From: m.From, To: m.To, Trust: m.Trust, Action: action, Detail: attemptsLine(res), Body: clip(m.Body)})
	state, detail, engineName := store.Reported, "", ""
	if err != nil {
		state, detail = store.Failed, err.Error()
	}
	if res != nil {
		engineName = res.Engine
	}
	if err := n.st.TransitionTask(m.ID, state, engineName, body, detail); err != nil {
		n.logf("task %s: %v", m.ID, err)
		return
	}
	if m.Kind != proto.Ask {
		return
	}
	if _, err := n.reply(ctx, m.ID, body); err != nil {
		n.logf("reply to %s: %v", m.ID, err)
	}
}

func routerPrompt(m *proto.Message) string {
	who := "your user's own machine " + m.From.Machine()
	if m.Trust == proto.Team {
		who = "a teammate (" + m.Sender.Login + "); you are in read-only mode"
	}
	return fmt.Sprintf("Request from %s via Agent Collaborator (sender %s). Do it, verify the result, and finish with a short "+
		"answer for the requester: verdict first, then evidence (paths, numbers, commands). Your final message is sent "+
		"back automatically.\n\n%s", who, m.From, strings.TrimSpace(m.Body))
}

func footer(machine string, res *engine.Result) string {
	s := fmt.Sprintf("answered by %s on %s", res.Engine, machine)
	if len(res.Attempts) > 1 {
		s += " (" + attemptsLine(res) + ")"
	}
	return s
}

func attemptsLine(res *engine.Result) string {
	if res == nil {
		return ""
	}
	var parts []string
	for _, a := range res.Attempts {
		p := a.Engine + ": " + a.Outcome
		if a.Detail != "" && a.Outcome != "ok" {
			p += " - " + a.Detail
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, "; ")
}

// clip truncates a message body for the audit/chats log.
func clip(s string) string {
	s = strings.TrimSpace(s)
	if len(s) > 2000 {
		return s[:2000] + "…"
	}
	return s
}

func tail(s string, n int) string {
	if len(s) > n {
		return "…" + s[len(s)-n:]
	}
	return s
}

func (n *Node) handlePeerWho(w http.ResponseWriter, r *http.Request) {
	_, trust, ok := n.identify(w, r)
	if !ok {
		return
	}
	writeJSON(w, n.who(r.Context(), trust))
}

// MachineInfo is what a machine tells peers about itself.
type MachineInfo struct {
	Name     string          `json:"name"`
	Version  string          `json:"version"`
	Sessions []store.Session `json:"sessions"`
	Engines  []string        `json:"engines"` // usable now, in router order
	Online   bool            `json:"online"`
	Error    string          `json:"error,omitempty"`
}

func (n *Node) who(ctx context.Context, trust proto.Trust) MachineInfo {
	info := MachineInfo{Name: n.cfg.Name, Version: Version, Online: true}
	n.prune(ctx)
	sessions, _ := n.st.Sessions()
	seen := map[string]bool{}
	for _, s := range sessions {
		if trust == proto.Owner || s.Team {
			s.Dir = "" // paths stay private
			info.Sessions = append(info.Sessions, s)
			seen[s.Name] = true
		}
	}
	if trust == proto.Owner { // every live Claude session on this machine, even unregistered ones
		for _, cs := range deliver.ScanClaude() {
			if cs.Name == "" || seen[cs.Name] {
				continue
			}
			info.Sessions = append(info.Sessions, store.Session{Name: cs.Name, Agent: "claude",
				Deliver: "claude:" + cs.SessionID, Detected: true, Seen: time.Unix(cs.UpdatedAt/1000, 0)})
			seen[cs.Name] = true
		}
	}
	for _, s := range n.router.Statuses(ctx) {
		if s.Ready && s.CoolUntil.IsZero() && (trust == proto.Owner || s.Limited) {
			info.Engines = append(info.Engines, s.Name)
		}
	}
	return info
}

// --- sending ---

// send delivers a message to its machine: locally, or to a peer, queueing it if the peer is offline.
func (n *Node) send(ctx context.Context, m *proto.Message) msgResult {
	if m.To.Machine() == n.cfg.Name {
		m.Sender, m.Trust = &proto.Identity{Login: n.owner, Machine: n.cfg.Name}, proto.Owner
		return n.accept(ctx, m)
	}
	if m.Kind == proto.Ask {
		_ = n.st.Asked(m.ID, m.To.Machine())
	}
	res, err := n.post(ctx, m)
	if err != nil {
		_ = n.st.Queue(&store.Outgoing{Msg: *m, Queued: time.Now(), Attempts: 1, LastErr: err.Error()})
		n.st.Audit(store.AuditEntry{Dir: "out", ID: m.ID, Kind: m.Kind, From: m.From, To: m.To, Action: "queued", Detail: err.Error(), Body: clip(m.Body)})
		return msgResult{ID: m.ID, State: "queued", Detail: m.To.Machine() + " unreachable, will retry: " + err.Error()}
	}
	n.st.Audit(store.AuditEntry{Dir: "out", ID: m.ID, Kind: m.Kind, From: m.From, To: m.To, Action: res.State, Detail: res.Detail, Body: clip(m.Body)})
	return res
}

// errPeer is a peer's definite answer (refused, bad request): not worth retrying.
type errPeer struct{ msg string }

func (e errPeer) Error() string { return e.msg }

func (n *Node) post(ctx context.Context, m *proto.Message) (msgResult, error) {
	b, _ := json.Marshal(m)
	req, _ := http.NewRequestWithContext(ctx, "POST", "http://"+m.To.Machine()+"/v1/msg", strings.NewReader(string(b)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set(machineHeader, n.cfg.Name) // used by local mode only
	req.Header.Set("X-Agent-Collaborator-From", string(m.From))
	resp, err := n.peers.Do(req)
	if err != nil {
		return msgResult{}, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if resp.StatusCode != http.StatusOK {
		// A definite no from the peer: report it, don't queue.
		return msgResult{ID: m.ID, State: "refused", Detail: strings.TrimSpace(string(body))}, nil
	}
	var res msgResult
	if err := json.Unmarshal(body, &res); err != nil {
		return msgResult{}, fmt.Errorf("bad answer from %s: %w", m.To.Machine(), err)
	}
	return res, nil
}

// reply answers a pending ask.
func (n *Node) reply(ctx context.Context, askID, body string) (msgResult, error) {
	if task, err := n.st.LoadTask(askID); err == nil && task.State == store.Running {
		if err := n.st.TransitionTask(askID, store.Reported, "", body, ""); err != nil {
			return msgResult{}, err
		}
	}
	p, err := n.st.TakePending(askID)
	if err != nil {
		return msgResult{}, err
	}
	m := &proto.Message{
		ID: proto.NewID(), Kind: proto.Reply, ReplyTo: askID,
		To: p.Msg.From, From: proto.Address(n.cfg.Name), Body: body,
		Hops: p.Msg.Hops + 1, Sent: time.Now(),
	}
	if m.Hops > proto.MaxHops {
		m.Hops = proto.MaxHops // a reply always gets through: it ends a chain, it doesn't extend it
	}
	if p.Msg.To.Session() != "" {
		m.From = proto.Address(n.cfg.Name + "/" + p.Msg.To.Session())
	}
	// A reply to a local ask lands straight in the reply store.
	if m.To.Machine() == n.cfg.Name {
		return msgResult{ID: m.ID, State: "delivered"}, n.st.PutReply(m)
	}
	return n.send(ctx, m), nil
}

// retryOutbox redelivers queued messages; they expire after a day.
func (n *Node) retryOutbox(ctx context.Context) {
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
		for _, o := range n.st.Outbox() {
			if time.Since(o.Queued) > 24*time.Hour {
				n.st.Dequeue(o.Msg.ID)
				n.st.Audit(store.AuditEntry{Dir: "out", ID: o.Msg.ID, Kind: o.Msg.Kind, To: o.Msg.To, Action: "expired", Detail: o.LastErr})
				continue
			}
			res, err := n.post(ctx, &o.Msg)
			if err != nil {
				o.Attempts++
				o.LastErr = err.Error()
				_ = n.st.Queue(&o)
				continue
			}
			n.st.Dequeue(o.Msg.ID)
			n.st.Audit(store.AuditEntry{Dir: "out", ID: o.Msg.ID, Kind: o.Msg.Kind, From: o.Msg.From, To: o.Msg.To, Action: res.State, Detail: "after retry: " + res.Detail})
		}
	}
}

// --- helpers ---

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// limiter allows n events per minute per key.
type limiter struct {
	mu   sync.Mutex
	per  int
	seen map[string][]time.Time
}

func newLimiter(perMinute int) *limiter {
	if perMinute <= 0 {
		perMinute = 30
	}
	return &limiter{per: perMinute, seen: map[string][]time.Time{}}
}

func (l *limiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	cut := time.Now().Add(-time.Minute)
	ts := l.seen[key][:0]
	for _, t := range l.seen[key] {
		if t.After(cut) {
			ts = append(ts, t)
		}
	}
	if len(ts) >= l.per {
		l.seen[key] = ts
		return false
	}
	l.seen[key] = append(ts, time.Now())
	return true
}

// interruptTasks surfaces unfinished work without replaying it.
func (n *Node) interruptTasks() error {
	tasks, err := n.st.ListTasks(0)
	if err != nil {
		return err
	}
	for _, task := range tasks {
		if task.State != store.Running {
			continue
		}
		if err := n.st.TransitionTask(task.ID, store.Interrupted, "", "", "daemon stopped while task was running"); err != nil {
			return err
		}
		n.logf("task %s interrupted at startup", task.ID)
	}
	return nil
}
