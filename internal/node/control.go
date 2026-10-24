package node

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/TheAdaply/agent-collaborator/internal/engine"
	"github.com/TheAdaply/agent-collaborator/internal/proto"
	"github.com/TheAdaply/agent-collaborator/internal/store"
)

// controlAPI is served on the local Unix socket (mode 0600): only this user's CLI, MCP servers and
// agents can call it. Everything it does is as the owner.
func (n *Node) controlAPI() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("POST /v1/send", n.ctlSend)
	mux.HandleFunc("GET /v1/wait/{id}", n.ctlWait)
	mux.HandleFunc("POST /v1/reply", n.ctlReply)
	mux.HandleFunc("GET /v1/inbox/{session}", n.ctlInbox)
	mux.HandleFunc("POST /v1/sessions", n.ctlRegister)
	mux.HandleFunc("DELETE /v1/sessions/{name}", n.ctlUnregister)
	mux.HandleFunc("GET /v1/who", n.ctlWho)
	mux.HandleFunc("GET /v1/fleet", n.ctlFleet)
	mux.HandleFunc("GET /v1/status", n.ctlStatus)
	mux.HandleFunc("GET /v1/engines", n.ctlEngines)
	mux.HandleFunc("POST /v1/engines/{name}/reset", n.ctlEngineReset)
	mux.HandleFunc("GET /v1/log", n.ctlLog)
	return mux
}

// SendRequest is the body of POST /v1/send.
type SendRequest struct {
	To     proto.Address `json:"to"`
	Body   string        `json:"body"`
	Kind   proto.Kind    `json:"kind"`             // ask (default) or tell
	From   string        `json:"from,omitempty"`   // sending session name, if any
	Prefer string        `json:"prefer,omitempty"` // engine hint for a router target
	Hops   int           `json:"hops,omitempty"`   // AC_HOPS of the calling task
}

func (n *Node) ctlSend(w http.ResponseWriter, r *http.Request) {
	var req SendRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, proto.MaxBody+64<<10)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if req.Kind == "" {
		req.Kind = proto.Ask
	}
	if req.Kind != proto.Ask && req.Kind != proto.Tell {
		http.Error(w, "kind must be ask or tell", http.StatusBadRequest)
		return
	}
	from := proto.Address(n.cfg.Name)
	if req.From != "" {
		from = proto.Address(n.cfg.Name + "/" + req.From)
	}
	to, err := n.resolve(r.Context(), req.To)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	m := &proto.Message{
		ID: proto.NewID(), Kind: req.Kind, To: to, From: from, Body: req.Body,
		Hops: req.Hops + 1, Sent: time.Now(), Prefer: req.Prefer,
	}
	if err := m.Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, n.send(r.Context(), m))
}

// resolve turns a friendly address into a concrete one (owner side only). A target with "/" or that
// names a known machine is left alone. A bare nickname (spaces allowed, e.g. "mac guy") is matched,
// case-insensitively, against every session name on this machine and on each online peer; a single
// match becomes <machine>/<session>. This is what lets akash just say "ask mac-guy ...".
func (n *Node) resolve(ctx context.Context, to proto.Address) (proto.Address, error) {
	raw := strings.TrimSpace(string(to))
	if raw == "" {
		return to, fmt.Errorf("empty address")
	}
	if strings.Contains(raw, "/") {
		return to, nil // already machine/session
	}
	nick := strings.ToLower(strings.ReplaceAll(raw, " ", "-"))
	if nick == n.cfg.Name {
		return proto.Address(nick), nil // this machine's router
	}
	peers, _ := n.tr.Peers(ctx)
	for _, p := range peers {
		if strings.EqualFold(p.Name, nick) {
			return proto.Address(nick), nil // a machine's router
		}
	}
	// A nickname: find the session(s) named nick across the network.
	type hit struct{ machine, session string }
	var hits []hit
	add := func(machine string, sessions []store.Session) {
		for _, s := range sessions {
			if strings.EqualFold(s.Name, nick) {
				hits = append(hits, hit{machine, s.Name})
			}
		}
	}
	add(n.cfg.Name, n.who(ctx, proto.Owner).Sessions)
	for _, p := range peers {
		if !p.Online {
			continue
		}
		if info, err := n.peerWho(ctx, p.Name); err == nil {
			add(p.Name, info.Sessions)
		}
	}
	switch len(hits) {
	case 1:
		return proto.Address(hits[0].machine + "/" + hits[0].session), nil
	case 0:
		return to, nil // leave it; delivery gives a clear "no session / no machine" error
	default:
		var names []string
		for _, h := range hits {
			names = append(names, h.machine+"/"+h.session)
		}
		return to, fmt.Errorf("%q is ambiguous: %s (use one)", raw, strings.Join(names, ", "))
	}
}

// ctlWait blocks until the reply to ask {id} arrives or ?timeout= seconds pass (default 600).
func (n *Node) ctlWait(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	secs, _ := strconv.Atoi(r.URL.Query().Get("timeout"))
	if secs <= 0 {
		secs = 600
	}
	deadline := time.Now().Add(time.Duration(secs) * time.Second)
	for {
		if m, ok := n.st.Reply(id); ok {
			writeJSON(w, m)
			return
		}
		if time.Now().After(deadline) {
			http.Error(w, "no reply yet to "+id, http.StatusGatewayTimeout)
			return
		}
		select {
		case <-r.Context().Done():
			return
		case <-time.After(400 * time.Millisecond):
		}
	}
}

func (n *Node) ctlReply(w http.ResponseWriter, r *http.Request) {
	var req struct{ ID, Body string }
	if err := json.NewDecoder(io.LimitReader(r.Body, proto.MaxBody+64<<10)).Decode(&req); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	res, err := n.reply(r.Context(), req.ID, req.Body)
	if err != nil {
		http.Error(w, err.Error(), http.StatusNotFound)
		return
	}
	writeJSON(w, res)
}

func (n *Node) ctlInbox(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("session")
	n.st.TouchSession(name)
	msgs, err := n.st.TakeInbox(name)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if msgs == nil {
		msgs = []proto.Message{}
	}
	writeJSON(w, msgs)
}

func (n *Node) ctlRegister(w http.ResponseWriter, r *http.Request) {
	var s store.Session
	if err := json.NewDecoder(r.Body).Decode(&s); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if s.Deliver == "" {
		s.Deliver = "inbox"
	}
	if err := (proto.Address(n.cfg.Name + "/" + s.Name)).Validate(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if err := n.st.PutSession(s); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]string{"address": n.cfg.Name + "/" + s.Name})
}

func (n *Node) ctlUnregister(w http.ResponseWriter, r *http.Request) {
	if err := n.st.RemoveSession(r.PathValue("name")); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	writeJSON(w, map[string]bool{"ok": true})
}

// ctlWho returns this machine and every online peer running Agent Collaborator, queried in parallel.
func (n *Node) ctlWho(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, n.fleetMachines(r.Context()))
}

func (n *Node) fleetMachines(parent context.Context) []MachineInfo {
	ctx, cancel := context.WithTimeout(parent, 6*time.Second)
	defer cancel()
	out := []MachineInfo{n.who(ctx, proto.Owner)}
	peers, err := n.tr.Peers(ctx)
	if err != nil {
		peers = nil
	}
	var mu sync.Mutex
	var wg sync.WaitGroup
	for _, p := range peers {
		info := MachineInfo{Name: p.Name}
		if !p.Online {
			mu.Lock()
			out = append(out, info)
			mu.Unlock()
			continue
		}
		wg.Add(1)
		go func(p Peer) {
			defer wg.Done()
			info, err := n.peerWho(ctx, p.Name)
			if err != nil {
				info = MachineInfo{Name: p.Name, Error: err.Error()}
			}
			mu.Lock()
			out = append(out, info)
			mu.Unlock()
		}(p)
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	for i := range out {
		sort.Slice(out[i].Sessions, func(a, b int) bool { return out[i].Sessions[a].Name < out[i].Sessions[b].Name })
	}
	return out
}

func (n *Node) peerWho(ctx context.Context, machine string) (MachineInfo, error) {
	req, _ := http.NewRequestWithContext(ctx, "GET", "http://"+machine+"/v1/who", nil)
	req.Header.Set(machineHeader, n.cfg.Name)
	resp, err := n.peers.Do(req)
	if err != nil {
		return MachineInfo{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		return MachineInfo{}, fmt.Errorf("%s", b)
	}
	var info MachineInfo
	err = json.NewDecoder(resp.Body).Decode(&info)
	return info, err
}

// Status is GET /v1/status.
type Status struct {
	Machine  string          `json:"machine"`
	Version  string          `json:"version"`
	Network  string          `json:"network"`
	Owner    string          `json:"owner"`
	Team     []string        `json:"team"`
	Sessions []store.Session `json:"sessions"`
	Outbox   int             `json:"outbox"`
	Engines  []engine.Status `json:"engines"`
}

func (n *Node) ctlStatus(w http.ResponseWriter, r *http.Request) {
	sessions, _ := n.st.Sessions()
	st := Status{Machine: n.cfg.Name, Version: Version, Network: string(n.cfg.Network), Owner: n.owner,
		Team: n.cfg.Trust.Team, Outbox: len(n.st.Outbox()), Engines: n.router.Statuses(r.Context())}
	for _, s := range sessions {
		st.Sessions = append(st.Sessions, s)
	}
	writeJSON(w, st)
}

func (n *Node) ctlEngines(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, n.router.Statuses(r.Context()))
}

func (n *Node) ctlEngineReset(w http.ResponseWriter, r *http.Request) {
	n.router.ClearCooldown(r.PathValue("name"))
	writeJSON(w, map[string]bool{"ok": true})
}

func (n *Node) ctlLog(w http.ResponseWriter, r *http.Request) {
	num, _ := strconv.Atoi(r.URL.Query().Get("n"))
	if num <= 0 {
		num = 30
	}
	writeJSON(w, n.st.AuditTail(num))
}
