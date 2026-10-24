package node

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"tailscale.com/client/local"
	"tailscale.com/ipn/ipnstate"
	"tailscale.com/tsnet"

	"github.com/TheAdaply/agent-collaborator/internal/config"
	"github.com/TheAdaply/agent-collaborator/internal/proto"
)

// Transport connects a node to its peers and says who is on the other end of a connection.
// Identity always comes from the network (Tailscale's WhoIs), never from what a peer claims.
type Transport interface {
	Listen() (net.Listener, error)
	// Identify returns who sent an incoming request.
	Identify(ctx context.Context, r *http.Request) (*proto.Identity, error)
	// Dial connects to a peer's Agent Collaborator port.
	Dial(ctx context.Context, machine string) (net.Conn, error)
	// Peers lists machines that may run Agent Collaborator.
	Peers(ctx context.Context) ([]Peer, error)
	// Login is the network login this node runs as (its owner, by default).
	Login(ctx context.Context) (string, error)
	// Name is this machine's name as peers see it ("" if the config's name is used as is).
	Name(ctx context.Context) (string, error)
	Close() error
}

// Peer is a machine on the network.
type Peer struct {
	Name   string `json:"name"`
	Login  string `json:"login"`
	Online bool   `json:"online"`
}

func newTransport(c *config.Config, logf func(string, ...any)) (Transport, error) {
	switch c.Network {
	case config.TSNet:
		s := &tsnet.Server{
			Hostname:  c.Name, // use tsnet on machines without the Tailscale app, or pick a name distinct from it
			Dir:       filepath.Join(c.StateDir, "tsnet"),
			Ephemeral: c.Ephemeral,
			UserLogf:  logf, // shows the login URL on first run
			Logf:      func(string, ...any) {},
		}
		lc, err := s.LocalClient()
		if err != nil {
			return nil, err
		}
		return &tailnet{c: c, ts: s, lc: lc}, nil
	case config.Tailscale:
		return &tailnet{c: c, lc: &local.Client{}}, nil
	case config.Local:
		return &loopback{c: c}, nil
	}
	return nil, fmt.Errorf("unknown network %q", c.Network)
}

// --- Tailscale (embedded tsnet or the system tailscaled) ---

// tailnet names every machine by its MagicDNS label, in both modes, so all nodes agree on names.
type tailnet struct {
	c  *config.Config
	ts *tsnet.Server // nil: use the system tailscaled
	lc *local.Client
}

func (t *tailnet) port() string { return strconv.Itoa(t.c.Port) }

func (t *tailnet) Listen() (net.Listener, error) {
	if t.ts != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
		defer cancel()
		if _, err := t.ts.Up(ctx); err != nil {
			return nil, fmt.Errorf("joining the tailnet: %w", err)
		}
		return t.ts.Listen("tcp", ":"+t.port())
	}
	st, err := t.lc.StatusWithoutPeers(context.Background())
	if err != nil {
		return nil, fmt.Errorf("tailscaled not reachable (is Tailscale running?): %w", err)
	}
	if st.BackendState != "Running" || len(st.TailscaleIPs) == 0 {
		return nil, fmt.Errorf("tailscale is %s; run: tailscale up", st.BackendState)
	}
	// Listen on the tailnet address only: never on the LAN or the internet.
	return net.Listen("tcp", net.JoinHostPort(st.TailscaleIPs[0].String(), t.port()))
}

func (t *tailnet) Identify(ctx context.Context, r *http.Request) (*proto.Identity, error) {
	w, err := t.lc.WhoIs(ctx, r.RemoteAddr)
	if err != nil {
		return nil, fmt.Errorf("whois %s: %w", r.RemoteAddr, err)
	}
	id := &proto.Identity{}
	if w.UserProfile != nil {
		id.Login = w.UserProfile.LoginName
	}
	if w.Node != nil {
		id.Machine = dnsLabel(w.Node.Name)
	}
	if id.Login == "" || id.Machine == "" {
		return nil, fmt.Errorf("whois %s: incomplete identity", r.RemoteAddr)
	}
	return id, nil
}

// dnsLabel is the first label of a MagicDNS name ("akashs-a35.tail1234.ts.net." -> "akashs-a35"): unlike the
// OS hostname ("akash's a35") it is always a valid, unique Agent Collaborator machine name.
func dnsLabel(fqdn string) string {
	l, _, _ := strings.Cut(fqdn, ".")
	return strings.ToLower(l)
}

func (t *tailnet) Dial(ctx context.Context, machine string) (net.Conn, error) {
	addr := net.JoinHostPort(machine, t.port())
	if t.ts != nil {
		return t.ts.Dial(ctx, "tcp", addr)
	}
	// System tailscaled: resolve through its status (MagicDNS may be off).
	ip, err := t.lookup(ctx, machine)
	if err != nil {
		return nil, err
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", net.JoinHostPort(ip, t.port()))
}

func (t *tailnet) lookup(ctx context.Context, machine string) (string, error) {
	st, err := t.lc.Status(ctx)
	if err != nil {
		return "", err
	}
	for _, p := range st.Peer {
		if dnsLabel(p.DNSName) == machine && len(p.TailscaleIPs) > 0 {
			return p.TailscaleIPs[0].String(), nil
		}
	}
	return "", fmt.Errorf("no machine %q on the tailnet", machine)
}

func (t *tailnet) Peers(ctx context.Context) ([]Peer, error) {
	st, err := t.lc.Status(ctx)
	if err != nil {
		return nil, err
	}
	var out []Peer
	for _, p := range st.Peer {
		if label := dnsLabel(p.DNSName); label != "" {
			out = append(out, Peer{Name: label, Login: userLogin(st, p), Online: p.Online})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func userLogin(st *ipnstate.Status, p *ipnstate.PeerStatus) string {
	if u, ok := st.User[p.UserID]; ok {
		return u.LoginName
	}
	return ""
}

func (t *tailnet) Login(ctx context.Context) (string, error) {
	st, err := t.lc.Status(ctx)
	if err != nil {
		return "", err
	}
	if st.Self == nil {
		return "", fmt.Errorf("not signed in to the tailnet")
	}
	if l := userLogin(st, st.Self); l != "" {
		return l, nil
	}
	return "", fmt.Errorf("own login unknown")
}

// Name: peers know this machine by its MagicDNS label, so that is its Agent Collaborator name too (the
// configured name can differ: the OS hostname may be "Mac.local", or Tailscale may have added "-1").
func (t *tailnet) Name(ctx context.Context) (string, error) {
	st, err := t.lc.StatusWithoutPeers(ctx)
	if err != nil || st.Self == nil {
		return "", err
	}
	return dnsLabel(st.Self.DNSName), nil
}

func (t *tailnet) Close() error {
	if t.ts != nil {
		return t.ts.Close()
	}
	return nil
}

// --- loopback, for development and tests ---

// loopback trusts the X-Agent-Collaborator-Machine header for the sender's name, so it only accepts
// connections from 127.0.0.1 and treats every sender as the owner. Never use it across machines.
type loopback struct{ c *config.Config }

const machineHeader = "X-Agent-Collaborator-Machine"

func (l *loopback) Listen() (net.Listener, error) {
	host, _, err := net.SplitHostPort(l.c.Listen)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("local network mode listens on loopback only, not %s", l.c.Listen)
	}
	return net.Listen("tcp", l.c.Listen)
}

func (l *loopback) Identify(_ context.Context, r *http.Request) (*proto.Identity, error) {
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("non-loopback connection in local mode")
	}
	m := r.Header.Get(machineHeader)
	if m == "" {
		return nil, fmt.Errorf("missing %s", machineHeader)
	}
	return &proto.Identity{Login: "local", Machine: m}, nil
}

func (l *loopback) Dial(ctx context.Context, machine string) (net.Conn, error) {
	addr, ok := l.c.Peers[machine]
	if !ok {
		return nil, fmt.Errorf("no peer %q in config [peers]", machine)
	}
	var d net.Dialer
	return d.DialContext(ctx, "tcp", addr)
}

func (l *loopback) Peers(ctx context.Context) ([]Peer, error) {
	var out []Peer
	for name, addr := range l.c.Peers {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			c.Close()
		}
		out = append(out, Peer{Name: name, Login: "local", Online: err == nil})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

func (l *loopback) Login(context.Context) (string, error) { return "local", nil }
func (l *loopback) Name(context.Context) (string, error)  { return "", nil }
func (l *loopback) Close() error                          { return nil }
