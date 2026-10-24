package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/TheAdaply/agent-collaborator/internal/client"
	"github.com/TheAdaply/agent-collaborator/internal/config"
	"github.com/TheAdaply/agent-collaborator/internal/node"
)

func cmdDoctor(ctx context.Context) error { return doctor(ctx, os.Stdout) }

func doctor(ctx context.Context, out io.Writer) error {
	failed := false
	check := func(name, status, hint string) {
		hint = strings.Join(strings.Fields(hint), " ")
		fmt.Fprintf(out, "%s %s: %s\n", name, status, hint)
		failed = failed || status == "FAIL"
	}
	dir, _ := config.Dirs()
	path := filepath.Join(dir, "config.toml")
	if _, err := os.Stat(path); err != nil {
		check("config", "FAIL", fmt.Sprintf("%v; run agent-collaborator init", err))
	}
	c, err := config.LoadReadOnly()
	if err != nil {
		check("config", "FAIL", err.Error()+"; fix config.toml")
		return fmt.Errorf("setup needs attention")
	}
	if !failed {
		check("config", "OK", path)
	}
	check("network", "OK", string(c.Network))
	check("machine", "OK", c.Name+" (configured name)")
	cl := client.New(c.Socket())
	probe, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	// This endpoint only inspects the existing transport; it never starts tsnet.
	var health node.NetworkHealth
	daemonErr := cl.Do(probe, "GET", "/v1/doctor/network", nil, &health)
	if daemonErr != nil {
		check("daemon", "FAIL", "not running or control API unavailable: "+daemonErr.Error()+"; run agent-collaborator up")
	} else {
		check("daemon", "OK", "control socket reachable")
		check("machine", "OK", health.Machine+" (daemon network name)")
	}
	if c.Network != config.Local {
		switch {
		case daemonErr != nil:
			check("tailnet", "FAIL", "cannot inspect membership; start the daemon and rerun doctor")
		case !health.Joined:
			check("tailnet", "FAIL", health.Error+"; check daemon login and Tailscale connectivity")
		case health.Error != "":
			check("tailnet", "FAIL", "joined, but peer discovery failed: "+health.Error)
		default:
			check("tailnet", "OK", "joined")
			if health.Peers == 0 {
				check("peers", "WARN", "no tailnet peers visible; connect another machine or check ACLs")
			} else {
				check("peers", "OK", fmt.Sprintf("%d tailnet devices visible", health.Peers))
			}
		}
	}
	rt, err := newRouter(c)
	if err != nil {
		check("engines", "FAIL", err.Error()+"; fix engine definitions")
	} else {
		// Statuses reads cooldowns without persisting expiry and runs only readiness probes.
		usable := 0
		for _, s := range rt.Statuses(ctx) {
			status, hint := "WARN", s.Detail
			switch {
			case !s.Installed:
				hint += "; install the engine CLI and check PATH"
			case !s.CoolUntil.IsZero():
				hint += "; wait until " + s.CoolUntil.Format(time.RFC3339)
			case !s.Ready:
				hint += "; sign in to the engine CLI"
			default:
				status, hint = "OK", "installed; readiness check passed"
				usable++
			}
			check("engine/"+s.Name, status, hint)
		}
		if usable == 0 {
			check("engines", "FAIL", "no usable engine; install and sign in to at least one configured engine")
		}
	}
	home, _ := os.UserHomeDir()
	service := ""
	switch runtime.GOOS {
	case "linux":
		service = filepath.Join(home, ".config", "systemd", "user", "agent-collaborator.service")
	case "darwin":
		service = filepath.Join(home, "Library", "LaunchAgents", "dev.agent-collaborator.plist")
	}
	if service == "" {
		check("service", "WARN", "service installation is supported on Linux and macOS")
	} else if _, err := os.Stat(service); err != nil {
		check("service", "WARN", "unit/plist unavailable; run agent-collaborator service install for login startup")
	} else {
		check("service", "OK", service+" present")
	}
	if failed {
		return fmt.Errorf("setup needs attention")
	}
	return nil
}
