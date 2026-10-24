// Package config loads Agent Collaborator's settings and engine definitions.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
)

// Network selects how a node reaches its peers.
type Network string

const (
	// TSNet embeds a Tailscale node in the daemon; nothing else to install.
	TSNet Network = "tsnet"
	// Tailscale uses the tailscaled already running on the machine.
	Tailscale Network = "tailscale"
	// Local is loopback only, for development and tests. Every connection counts as the owner.
	Local Network = "local"
)

// Config is ~/.config/agent-collaborator/config.toml.
type Config struct {
	Name    string  `toml:"name"`    // this machine's name on the network
	Network Network `toml:"network"` // tsnet | tailscale | local
	Port    int     `toml:"port"`    // peer port on the tailnet
	// Ephemeral (tsnet only): Tailscale removes this node soon after it goes offline. For tests.
	Ephemeral bool     `toml:"ephemeral"`
	Engines   []string `toml:"engines"` // router order; the first usable engine runs a task
	Workdir   string   `toml:"workdir"` // where router tasks run

	// Owner is the network login that counts as you (e.g. "akash@github"). Empty: the login this
	// node itself is signed in as.
	Owner string `toml:"owner"`
	Trust Trust  `toml:"trust"`

	// Local mode only: peer name -> host:port, and this node's listen address.
	Listen string            `toml:"listen"`
	Peers  map[string]string `toml:"peers"`

	// TaskTimeout bounds one engine run.
	TaskTimeout Duration `toml:"task_timeout"`

	Dir      string `toml:"-"` // config dir
	StateDir string `toml:"-"` // state dir
}

// Trust maps senders to levels.
type Trust struct {
	Team []string `toml:"team"` // logins granted Team trust
	// RatePerMinute caps messages accepted from one sender.
	RatePerMinute int `toml:"rate_per_minute"`
}

// Duration is a time.Duration that reads from TOML strings like "45m".
type Duration struct{ time.Duration }

// UnmarshalText implements encoding.TextUnmarshaler.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := time.ParseDuration(string(b))
	d.Duration = v
	return err
}

// Dirs returns the config and state directories, honouring AC_HOME (one tree for both).
func Dirs() (cfgDir, stateDir string) {
	if h := os.Getenv("AC_HOME"); h != "" {
		return h, filepath.Join(h, "state")
	}
	home, _ := os.UserHomeDir()
	cfgDir = filepath.Join(home, ".config", "agent-collaborator")
	if x := os.Getenv("XDG_CONFIG_HOME"); x != "" {
		cfgDir = filepath.Join(x, "agent-collaborator")
	}
	stateDir = filepath.Join(home, ".local", "state", "agent-collaborator")
	if x := os.Getenv("XDG_STATE_HOME"); x != "" {
		stateDir = filepath.Join(x, "agent-collaborator")
	}
	return cfgDir, stateDir
}

// Default returns the settings used when there is no config file.
func Default() *Config {
	host, _ := os.Hostname()
	home, _ := os.UserHomeDir()
	return &Config{
		Name:        sanitize(host),
		Network:     TSNet,
		Port:        7433,
		Engines:     []string{"claude", "codex", "gemini"},
		Workdir:     home,
		Trust:       Trust{RatePerMinute: 30},
		TaskTimeout: Duration{45 * time.Minute},
	}
}

// Load reads the config file (if any) over the defaults.
func Load() (*Config, error) {
	c := Default()
	c.Dir, c.StateDir = Dirs()
	path := filepath.Join(c.Dir, "config.toml")
	if _, err := os.Stat(path); err == nil {
		md, err := toml.DecodeFile(path, c)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", path, err)
		}
		if un := md.Undecoded(); len(un) > 0 {
			return nil, fmt.Errorf("%s: unknown setting %q", path, un[0].String())
		}
	}
	c.Workdir = expandHome(c.Workdir)
	c.Name = sanitize(c.Name)
	switch c.Network {
	case TSNet, Tailscale, Local:
	default:
		return nil, fmt.Errorf("network = %q: want tsnet, tailscale or local", c.Network)
	}
	if c.Network == Local && c.Listen == "" {
		c.Listen = fmt.Sprintf("127.0.0.1:%d", c.Port)
	}
	if err := os.MkdirAll(c.StateDir, 0o700); err != nil {
		return nil, err
	}
	return c, nil
}

// Socket is the local control socket the CLI and MCP server talk to.
func (c *Config) Socket() string { return filepath.Join(c.StateDir, "agent-collaborator.sock") }

// EnginesDir holds user engine definitions that add to or override the built-in ones.
func (c *Config) EnginesDir() string { return filepath.Join(c.Dir, "engines") }

func expandHome(p string) string {
	if p == "~" || strings.HasPrefix(p, "~/") {
		home, _ := os.UserHomeDir()
		return filepath.Join(home, strings.TrimPrefix(p, "~"))
	}
	return p
}

// sanitize turns a hostname into a valid machine name.
func sanitize(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == ' ':
			b.WriteRune('-')
		}
	}
	if b.Len() == 0 {
		return "machine"
	}
	return b.String()
}
