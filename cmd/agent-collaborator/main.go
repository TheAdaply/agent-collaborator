// Command Agent Collaborator connects AI coding agents across your machines (and your team's) over
// Tailscale, and routes each task to the first AI engine that still has credit.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"github.com/TheAdaply/agent-collaborator/internal/command"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/TheAdaply/agent-collaborator/internal/client"
	"github.com/TheAdaply/agent-collaborator/internal/config"
	"github.com/TheAdaply/agent-collaborator/internal/engine"
	"github.com/TheAdaply/agent-collaborator/internal/mcp"
	"github.com/TheAdaply/agent-collaborator/internal/node"
	"github.com/TheAdaply/agent-collaborator/internal/proto"
	"github.com/TheAdaply/agent-collaborator/internal/store"
)

var usage = command.Text(`@COMMAND@ - Agent Collaborator: your AI agents, talking to each other across machines, never stuck on one subscription.

Talk
  @COMMAND@ ask [TO] "task"        ask a machine or session and wait for the answer
                                   (no TO: this machine's router - Claude, then Codex, ...)
  @COMMAND@ send TO "message"      fire and forget
  @COMMAND@ reply ID "answer"      answer an ask you received ("-" reads the answer from stdin)
  @COMMAND@ who                    machines, their agent sessions and engines
  @COMMAND@ fleet                  fleet inventory and recent activity

Engines
  @COMMAND@ run "task"             run a task here on the first usable engine (no daemon needed)
  @COMMAND@ engines [--reset NAME] engine order and state; --reset clears an out-of-usage mark

Sessions
  @COMMAND@ register NAME --claude ID|NAME | --tmux SOCKET:TARGET | --inbox [--team] [--agent A]
  @COMMAND@ unregister NAME
  @COMMAND@ inbox NAME             take a session's waiting messages
  @COMMAND@ setup claude|codex|gemini   give that agent the Agent Collaborator tools (MCP)

Daemon
  @COMMAND@ init [--network tsnet|tailscale|local]   write a starter config
  @COMMAND@ up                     run the daemon in the foreground
  @COMMAND@ service install|remove run it at login (systemd --user / launchd)
  @COMMAND@ doctor                 check setup without changing it
  @COMMAND@ status | log [-n N] | chats [-n N] | version
  @COMMAND@ chats                  the actual messages agents/machines have exchanged (conversation view)

TO is "machine" or "machine/session". Docs: https://github.com/TheAdaply/agent-collaborator`)

func main() {
	if len(os.Args) < 2 {
		fmt.Println(usage)
		return
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "ask":
		err = cmdAsk(ctx, args)
	case "send", "tell":
		err = cmdSend(ctx, args)
	case "reply":
		err = cmdReply(ctx, args)
	case "fleet":
		err = withArgs(args, 0, command.Text("@COMMAND@ fleet"), func() error { return cmdFleet(ctx) })
	case "who":
		err = cmdWho(ctx)
	case "run":
		err = cmdRun(ctx, args)
	case "engines":
		err = cmdEngines(ctx, args)
	case "register":
		err = cmdRegister(ctx, args)
	case "unregister":
		err = withArgs(args, 1, command.Text("@COMMAND@ unregister NAME"), func() error {
			return cli().Do(ctx, "DELETE", "/v1/sessions/"+args[0], nil, nil)
		})
	case "inbox":
		err = cmdInbox(ctx, args)
	case "setup":
		err = cmdSetup(args)
	case "init":
		err = cmdInit(args)
	case "up", "daemon":
		err = cmdUp(ctx)
	case "service":
		err = cmdService(args)
	case "doctor":
		err = withArgs(args, 0, command.Text("@COMMAND@ doctor"), func() error { return cmdDoctor(ctx) })
	case "status":
		err = cmdStatus(ctx)
	case "log":
		err = cmdLog(ctx, args)
	case "chats", "chat":
		err = cmdChats(ctx, args)
	case "mcp":
		err = cmdMCP(ctx, args)
	case "version", "--version", "-v":
		fmt.Println("Agent Collaborator", node.Version)
	case "help", "--help", "-h":
		fmt.Println(usage)
	default:
		err = fmt.Errorf(command.Text("unknown command %q (@COMMAND@ help)"), cmd)
	}
	if err != nil {
		fmt.Fprintln(os.Stderr, command.Name()+":", err)
		os.Exit(1)
	}
}

func mustConfig() *config.Config {
	c, err := config.Load()
	if err != nil {
		fmt.Fprintln(os.Stderr, command.Name()+":", err)
		os.Exit(1)
	}
	return c
}

func cli() *client.Client { return client.New(mustConfig().Socket()) }

// parse handles flags anywhere among the arguments (Go's flag package stops at the first
// positional one) and returns the positional arguments.
func parse(fs *flag.FlagSet, args []string) []string {
	fs.SetOutput(io.Discard)
	var flags, pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		flags = append(flags, a)
		name := strings.TrimLeft(a, "-")
		if strings.Contains(name, "=") {
			continue
		}
		if f := fs.Lookup(name); f != nil {
			if b, ok := f.Value.(interface{ IsBoolFlag() bool }); !(ok && b.IsBoolFlag()) && i+1 < len(args) {
				flags = append(flags, args[i+1])
				i++
			}
		}
	}
	if err := fs.Parse(flags); err != nil {
		fmt.Fprintln(os.Stderr, command.Name()+":", err)
		os.Exit(2)
	}
	return pos
}

func withArgs(args []string, n int, use string, f func() error) error {
	if len(args) < n {
		return fmt.Errorf("usage: %s", use)
	}
	return f()
}

func hopsFromEnv() int {
	h, _ := strconv.Atoi(os.Getenv("AC_HOPS"))
	return h
}

func cmdAsk(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("ask", flag.ExitOnError)
	prefer := fs.String("prefer", "", "engine to try first")
	timeout := fs.Duration("timeout", 30*time.Minute, "how long to wait for the answer")
	from := fs.String("from", os.Getenv("AC_SESSION"), "sending session name")
	pos := parse(fs, args)
	var to, msg string
	switch len(pos) {
	case 1:
		to, msg = mustConfig().Name, pos[0]
	case 2:
		to, msg = pos[0], pos[1]
	default:
		return fmt.Errorf("%s", command.Text(`usage: @COMMAND@ ask [TO] "task"`))
	}
	c := cli()
	var res struct{ ID, State, Detail string }
	if err := c.Do(ctx, "POST", "/v1/send", node.SendRequest{To: proto.Address(to), Body: msg, Kind: proto.Ask,
		Prefer: *prefer, From: *from, Hops: hopsFromEnv()}, &res); err != nil {
		return err
	}
	switch res.State {
	case "refused", "error":
		return fmt.Errorf("%s: %s", res.State, res.Detail)
	}
	fmt.Fprintf(os.Stderr, "%s %s: %s\n", res.ID, res.State, res.Detail)
	var reply proto.Message
	if err := c.Wait(ctx, res.ID, *timeout, &reply); err != nil {
		return fmt.Errorf(command.Text("no answer yet (%v); the answer will still arrive - check: @COMMAND@ log"), err)
	}
	fmt.Println(reply.Body)
	return nil
}

func cmdSend(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("send", flag.ExitOnError)
	from := fs.String("from", os.Getenv("AC_SESSION"), "sending session name")
	pos := parse(fs, args)
	if len(pos) != 2 {
		return fmt.Errorf("%s", command.Text(`usage: @COMMAND@ send TO "message"`))
	}
	var res struct{ ID, State, Detail string }
	if err := cli().Do(ctx, "POST", "/v1/send", node.SendRequest{To: proto.Address(pos[0]), Body: pos[1],
		Kind: proto.Tell, From: *from, Hops: hopsFromEnv()}, &res); err != nil {
		return err
	}
	fmt.Printf("%s %s: %s\n", res.ID, res.State, res.Detail)
	if res.State == "refused" || res.State == "error" {
		os.Exit(1)
	}
	return nil
}

func cmdReply(ctx context.Context, args []string) error {
	if len(args) != 2 {
		return fmt.Errorf("%s", command.Text(`usage: @COMMAND@ reply ID "answer"   (or - to read the answer from stdin)`))
	}
	body := args[1]
	if body == "-" {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, proto.MaxBody))
		if err != nil {
			return err
		}
		body = string(b)
	}
	var res struct{ ID, State, Detail string }
	if err := cli().Do(ctx, "POST", "/v1/reply", map[string]string{"ID": args[0], "Body": body}, &res); err != nil {
		return err
	}
	fmt.Printf("replied (%s): %s %s\n", res.ID, res.State, res.Detail)
	return nil
}

func cmdWho(ctx context.Context) error {
	var ms []node.MachineInfo
	if err := cli().Do(ctx, "GET", "/v1/who", nil, &ms); err != nil {
		return err
	}
	fmt.Println(mcp.FormatWho(ms, os.Getenv("AC_SESSION")))
	return nil
}

// cmdRun is the router on its own: no daemon, no network.
func cmdRun(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	dir := fs.String("dir", ".", "directory to run the task in")
	prefer := fs.String("prefer", "", "engine to try first")
	verbose := fs.Bool("v", false, "show every engine attempt")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("%s", command.Text(`usage: @COMMAND@ run [--dir D] [--prefer ENGINE] "task"   ("-" reads the task from stdin)`))
	}
	task := pos[0]
	if task == "-" {
		b, err := io.ReadAll(io.LimitReader(os.Stdin, proto.MaxBody))
		if err != nil {
			return err
		}
		task = string(b)
	}
	rt, err := newRouter(mustConfig())
	if err != nil {
		return err
	}
	abs, _ := filepath.Abs(*dir)
	res, err := rt.Run(ctx, engine.Task{ID: proto.NewID(), Prompt: task, Dir: abs, Prefer: *prefer})
	if res != nil {
		for _, a := range res.Attempts {
			if *verbose || a.Outcome != "ok" {
				fmt.Fprintf(os.Stderr, "[%s: %s%s]\n", a.Engine, a.Outcome, ifNotEmpty(" - ", a.Detail))
			}
		}
		if res.Output != "" {
			fmt.Println(res.Output)
		}
	}
	return err
}

func ifNotEmpty(sep, s string) string {
	if s == "" {
		return ""
	}
	return sep + s
}

func newRouter(c *config.Config) (*engine.Router, error) {
	defs, err := engine.Load(c.EnginesDir())
	if err != nil {
		return nil, err
	}
	return engine.NewRouter(c.Engines, defs, filepath.Join(c.StateDir, "engines.json"), c.TaskTimeout.Duration)
}

func cmdEngines(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("engines", flag.ExitOnError)
	reset := fs.String("reset", "", "put an out-of-usage engine back into rotation")
	parse(fs, args)
	c := mustConfig()
	cl := client.New(c.Socket())
	if *reset != "" {
		if err := cl.Do(ctx, "POST", "/v1/engines/"+*reset+"/reset", nil, nil); errors.Is(err, client.ErrNoDaemon) {
			rt, err := newRouter(c)
			if err != nil {
				return err
			}
			rt.ClearCooldown(*reset)
		} else if err != nil {
			return err
		}
	}
	var st node.Status
	if err := cl.Do(ctx, "GET", "/v1/status", nil, &st); errors.Is(err, client.ErrNoDaemon) {
		rt, err := newRouter(c) // works without the daemon too
		if err != nil {
			return err
		}
		st.Engines = rt.Statuses(ctx)
	} else if err != nil {
		return err
	}
	fmt.Println(mcp.FormatEngines(st))
	return nil
}

func cmdRegister(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("register", flag.ExitOnError)
	tmux := fs.String("tmux", "", "deliver by typing into this tmux pane: SOCKET:TARGET (SOCKET may be empty)")
	claude := fs.String("claude", "", "deliver by posting to a Claude Code session (its id or name; wakes idle turns)")
	fallback := fs.String("fallback", "", "tmux SOCKET:TARGET to type into if --claude push fails")
	inbox := fs.Bool("inbox", false, command.Text("the session pulls its messages (MCP check_inbox / @COMMAND@ inbox)"))
	router := fs.Bool("router", false, "hand this session's messages to the engine router")
	team := fs.Bool("team", false, "let teammates message this session")
	agent := fs.String("agent", "", "claude, codex, ... (shown in who)")
	pos := parse(fs, args)
	if len(pos) != 1 {
		return fmt.Errorf("%s", command.Text("usage: @COMMAND@ register NAME --tmux SOCKET:TARGET | --inbox | --router [--team] [--agent A]"))
	}
	s := store.Session{Name: pos[0], Team: *team, Agent: *agent, Fallback: ""}
	if *fallback != "" {
		s.Fallback = "tmux:" + *fallback
	}
	switch {
	case *claude != "":
		s.Deliver = "claude:" + *claude
	case *tmux != "":
		s.Deliver = "tmux:" + *tmux
	case *router:
		s.Deliver = "router"
	case *inbox:
		s.Deliver = "inbox"
	default:
		return fmt.Errorf("say how to deliver: --claude ID|NAME, --tmux SOCKET:TARGET, --inbox or --router")
	}
	s.Dir, _ = os.Getwd()
	var out map[string]string
	if err := cli().Do(ctx, "POST", "/v1/sessions", s, &out); err != nil {
		return err
	}
	fmt.Println("registered", out["address"])
	return nil
}

func cmdInbox(ctx context.Context, args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%s", command.Text("usage: @COMMAND@ inbox SESSION"))
	}
	var msgs []proto.Message
	if err := cli().Do(ctx, "GET", "/v1/inbox/"+args[0], nil, &msgs); err != nil {
		return err
	}
	if len(msgs) == 0 {
		fmt.Println("No messages.")
	}
	for _, m := range msgs {
		fmt.Println(proto.Frame(&m))
		fmt.Println()
	}
	return nil
}

func cmdStatus(ctx context.Context) error {
	var st node.Status
	if err := cli().Do(ctx, "GET", "/v1/status", nil, &st); err != nil {
		return err
	}
	fmt.Printf("machine  %s (Agent Collaborator %s, network %s)\nowner    %s\n", st.Machine, st.Version, st.Network, st.Owner)
	if len(st.Team) > 0 {
		fmt.Printf("team     %s\n", strings.Join(st.Team, ", "))
	}
	fmt.Printf("outbox   %d waiting\nsessions %d\n", st.Outbox, len(st.Sessions))
	for _, s := range st.Sessions {
		fmt.Printf("  %s (%s)\n", s.Name, s.Deliver)
	}
	fmt.Println("engines")
	for _, l := range strings.Split(mcp.FormatEngines(st), "\n") {
		fmt.Println("  " + l)
	}
	return nil
}

func cmdLog(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("log", flag.ExitOnError)
	n := fs.Int("n", 20, "entries")
	asJSON := fs.Bool("json", false, "raw JSON lines")
	parse(fs, args)
	var es []store.AuditEntry
	if err := cli().Do(ctx, "GET", fmt.Sprintf("/v1/log?n=%d", *n), nil, &es); err != nil {
		return err
	}
	for _, e := range es {
		if *asJSON {
			b, _ := json.Marshal(e)
			fmt.Println(string(b))
			continue
		}
		fmt.Printf("%s %-3s %-5s %-24s %s -> %s  %s%s\n", e.Time.Local().Format("01-02 15:04:05"), e.Dir, e.Kind,
			e.ID, e.From, e.To, e.Action, ifNotEmpty(": ", e.Detail))
	}
	return nil
}

func cmdChats(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("chats", flag.ExitOnError)
	n := fs.Int("n", 40, "how many recent messages to show")
	parse(fs, args)
	var es []store.AuditEntry
	if err := cli().Do(ctx, "GET", fmt.Sprintf("/v1/log?n=%d", *n*3), nil, &es); err != nil {
		return err
	}
	shown := 0
	for _, e := range es {
		if e.Body == "" { // skip pure delivery/metadata rows; those are in `Agent Collaborator log`
			continue
		}
		arrow := e.From
		if e.From == "" {
			arrow = "(local)"
		}
		fmt.Printf("\n%s  %s → %s  [%s]\n", e.Time.Local().Format("01-02 15:04:05"), arrow, e.To, e.Kind)
		for _, line := range strings.Split(strings.TrimRight(e.Body, "\n"), "\n") {
			fmt.Println("    " + line)
		}
		shown++
	}
	if shown == 0 {
		fmt.Println("No message text recorded yet. New messages from here on will appear (older ones were metadata-only).")
	}
	return nil
}

func cmdUp(ctx context.Context) error {
	n, err := node.New(mustConfig())
	if err != nil {
		return err
	}
	return n.Run(ctx)
}

func cmdMCP(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("mcp", flag.ExitOnError)
	session := fs.String("session", os.Getenv("AC_SESSION"), "this session's name on the network")
	agent := fs.String("agent", "", "claude, codex, ...")
	team := fs.Bool("team", false, "let teammates message this session")
	parse(fs, args)
	wd, _ := os.Getwd()
	if *session == "" {
		*session = defaultSession(*agent, wd)
	}
	s := &mcp.Server{C: cli(), Session: *session, Agent: *agent, Dir: wd, Team: *team, Hops: hopsFromEnv()}
	return s.Serve(ctx, os.Stdin, os.Stdout)
}

// defaultSession names a session after its agent and folder, e.g. "claude-api".
func defaultSession(agent, dir string) string {
	if agent == "" {
		agent = "agent"
	}
	base := filepath.Base(dir)
	if home, _ := os.UserHomeDir(); dir == home {
		base = "home"
	}
	var b strings.Builder
	for _, r := range strings.ToLower(agent + "-" + base) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			b.WriteRune(r)
		} else {
			b.WriteRune('-')
		}
	}
	return b.String()
}

func cmdFleet(ctx context.Context) error {
	var fleet node.FleetInfo
	if err := cli().Do(ctx, "GET", "/v1/fleet", nil, &fleet); err != nil {
		return err
	}
	fmt.Print(node.FormatFleet(fleet))
	return nil
}
