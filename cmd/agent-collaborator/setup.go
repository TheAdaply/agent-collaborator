package main

import (
	"flag"
	"fmt"
	"github.com/TheAdaply/agent-collaborator/internal/command"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/TheAdaply/agent-collaborator/internal/config"
)

var configTemplate = command.Text(`# Agent Collaborator config. Docs: https://github.com/TheAdaply/agent-collaborator#config

# This machine's name on the network (others address it as "%[1]s" or "%[1]s/<session>").
name = "%[1]s"

# tsnet: Agent Collaborator joins your tailnet itself (nothing else to install; first run prints a login link)
# tailscale: use the Tailscale app/daemon already running here
# local: loopback only, for trying it out on one machine
network = "%[2]s"
port = 7433

# Router order: a task goes to the first engine that is installed, signed in and not out of usage.
# Add your own engines as TOML files in %[3]s.
engines = ["claude", "codex", "gemini"]

# Where tasks sent to this machine (address "%[1]s") run.
workdir = "~"
task_timeout = "45m"

# Who counts as you: by default, the login this machine uses on the tailnet.
# owner = "you@github"

[trust]
# Teammates (tailnet logins) who may message sessions you share (@COMMAND@ register --team)
# and run read-only tasks on this machine's router.
team = []
rate_per_minute = 30
`)

func cmdInit(args []string) error {
	fs := flag.NewFlagSet("init", flag.ExitOnError)
	network := fs.String("network", "tsnet", "tsnet | tailscale | local")
	force := fs.Bool("force", false, "overwrite an existing config")
	parse(fs, args)
	cfgDir, _ := config.Dirs()
	path := filepath.Join(cfgDir, "config.toml")
	if _, err := os.Stat(path); err == nil && !*force {
		return fmt.Errorf("%s exists (use --force to overwrite)", path)
	}
	if err := os.MkdirAll(filepath.Join(cfgDir, "engines"), 0o700); err != nil {
		return err
	}
	name := config.Default().Name
	if err := os.WriteFile(path, []byte(fmt.Sprintf(configTemplate, name, *network, filepath.Join(cfgDir, "engines"))), 0o600); err != nil {
		return err
	}
	fmt.Printf(command.Text(`wrote %s (machine %q, network %s)

next:
  @COMMAND@ up                      start the daemon (first run prints a Tailscale login link)
  @COMMAND@ service install         or: run it at login, in the background
  @COMMAND@ setup claude            give Claude Code the Agent Collaborator tools (also: codex, gemini)
  @COMMAND@ engines                 see which AI engines are ready
`), path, name, *network)
	return nil
}

func self() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func cmdService(args []string) error {
	if len(args) != 1 || (args[0] != "install" && args[0] != "remove") {
		return fmt.Errorf("%s", command.Text("usage: @COMMAND@ service install|remove"))
	}
	exe, err := self()
	if err != nil {
		return err
	}
	home, _ := os.UserHomeDir()
	switch runtime.GOOS {
	case "linux":
		unit := filepath.Join(home, ".config", "systemd", "user", "agent-collaborator.service")
		if args[0] == "remove" {
			_ = run("systemctl", "--user", "disable", "--now", "agent-collaborator")
			return os.Remove(unit)
		}
		// Engines (claude, codex, ...) must be on the service's PATH: use the one from this shell.
		body := fmt.Sprintf(`[Unit]
Description=Agent Collaborator: agents across machines, routed to the engine with credit
After=network-online.target

[Service]
ExecStart=%s up
Environment=PATH=%s
Restart=always
RestartSec=5

[Install]
WantedBy=default.target
`, exe, os.Getenv("PATH"))
		if err := os.MkdirAll(filepath.Dir(unit), 0o755); err != nil {
			return err
		}
		if err := os.WriteFile(unit, []byte(body), 0o644); err != nil {
			return err
		}
		if err := run("systemctl", "--user", "daemon-reload"); err != nil {
			return err
		}
		if err := run("systemctl", "--user", "enable", "--now", "agent-collaborator"); err != nil {
			return err
		}
		fmt.Println("installed", unit, "- logs: journalctl --user -u agent-collaborator -f")
		fmt.Println("to keep it running while logged out: sudo loginctl enable-linger", os.Getenv("USER"))
	case "darwin":
		plist := filepath.Join(home, "Library", "LaunchAgents", "dev.agent-collaborator.plist")
		domain := fmt.Sprintf("gui/%d", os.Getuid())
		if args[0] == "remove" {
			_ = run("launchctl", "bootout", domain, plist)
			return os.Remove(plist)
		}
		logf := filepath.Join(home, "Library", "Logs", "agent-collaborator.log")
		body := fmt.Sprintf(`<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0"><dict>
  <key>Label</key><string>dev.agent-collaborator</string>
  <key>ProgramArguments</key><array><string>%s</string><string>up</string></array>
  <key>EnvironmentVariables</key><dict><key>PATH</key><string>%s</string></dict>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>%s</string>
  <key>StandardErrorPath</key><string>%s</string>
</dict></plist>
`, exe, os.Getenv("PATH"), logf, logf)
		if err := os.WriteFile(plist, []byte(body), 0o644); err != nil {
			return err
		}
		_ = run("launchctl", "bootout", domain, plist)
		if err := run("launchctl", "bootstrap", domain, plist); err != nil {
			return err
		}
		fmt.Println("installed", plist, "- logs:", logf)
	default:
		return fmt.Errorf(command.Text("service install supports Linux and macOS; on %s run `@COMMAND@ up` yourself"), runtime.GOOS)
	}
	return nil
}

// cmdSetup registers Agent Collaborator's MCP server with an agent, using the agent's own CLI.
func cmdSetup(args []string) error {
	if len(args) != 1 {
		return fmt.Errorf("%s", command.Text("usage: @COMMAND@ setup claude|codex|gemini"))
	}
	exe, err := self()
	if err != nil {
		return err
	}
	var cmd []string
	switch args[0] {
	case "claude":
		cmd = []string{"claude", "mcp", "add", "--scope", "user", "agent-collaborator", "--", exe, "mcp", "--agent", "claude"}
	case "codex":
		cmd = []string{"codex", "mcp", "add", "agent-collaborator", "--", exe, "mcp", "--agent", "codex"}
	case "gemini":
		cmd = []string{"gemini", "mcp", "add", "--scope", "user", "agent-collaborator", exe, "mcp", "--agent", "gemini"}
	default:
		return fmt.Errorf("unknown agent %q: add `%s mcp --agent NAME` as a stdio MCP server in its settings", args[0], exe)
	}
	fmt.Println("$", strings.Join(cmd, " "))
	if err := run(cmd[0], cmd[1:]...); err != nil {
		return err
	}
	fmt.Printf("done: new %s sessions get the Agent Collaborator tools (who, send, check_inbox, reply, engines).\n", args[0])
	fmt.Println("each session is named <agent>-<folder>; set AC_SESSION to choose its name.")
	return nil
}

func run(name string, args ...string) error {
	c := exec.Command(name, args...)
	c.Stdout, c.Stderr = os.Stdout, os.Stderr
	return c.Run()
}
