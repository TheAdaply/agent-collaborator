package node

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/TheAdaply/agent-collaborator/internal/proto"
	"github.com/TheAdaply/agent-collaborator/internal/store"
)

// FleetInfo is the owner-visible inventory and this machine's recent audit activity.
type FleetInfo struct {
	Machine   string             `json:"machine"`
	Generated time.Time          `json:"generated"`
	Machines  []MachineInfo      `json:"machines"`
	Activity  []store.AuditEntry `json:"activity"`
}

func (n *Node) fleet(r *http.Request, count int) FleetInfo {
	return FleetInfo{Machine: n.cfg.Name, Generated: time.Now(), Machines: n.fleetMachines(r.Context()), Activity: n.st.AuditTail(count)}
}

func (n *Node) ctlFleet(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, n.fleet(r, 15))
}

func (n *Node) handleFleet(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Header().Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; base-uri 'none'; frame-ancestors 'none'")
	id, err := n.tr.Identify(r.Context(), r)
	if err != nil || id == nil || id.Login == "" || n.owner == "" || n.trustOf(id.Login) != proto.Owner {
		http.Error(w, "owner only", http.StatusForbidden)
		return
	}
	var buf bytes.Buffer
	if err := fleetPage.Execute(&buf, n.fleet(r, 30)); err != nil {
		n.logf("render fleet: %v", err)
		http.Error(w, "cannot render fleet", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(buf.Bytes())
}

func fleetTime(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	return t.UTC().Format(time.RFC3339)
}

func fleetSnippet(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > 180 {
		return string(r[:180]) + "…"
	}
	return s
}

// FormatFleet produces stable text ordering from the shared fleet snapshot.
func FormatFleet(f FleetInfo) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fleet via %s · %s\nConversations are in Claude Remote Control.\n", f.Machine, fleetTime(f.Generated))
	for _, m := range f.Machines {
		state := "offline"
		if m.Online {
			state = "online"
		}
		fmt.Fprintf(&b, "\n%s [%s]\n", m.Name, state)
		if m.Error != "" {
			fmt.Fprintf(&b, "  Inventory unavailable: %s\n", fleetSnippet(m.Error))
		}
		fmt.Fprintf(&b, "  Usable engines: %s\n", strings.Join(m.Engines, ", "))
		for _, s := range m.Sessions {
			source := "registered"
			if s.Detected {
				source = "detected"
			}
			fmt.Fprintf(&b, "  %s · %s · %s · last seen %s\n", fleetSnippet(s.Name), fleetSnippet(s.Agent), source, fleetTime(s.Seen))
		}
		if len(m.Sessions) == 0 {
			fmt.Fprintln(&b, "  No sessions reported.")
		}
	}
	fmt.Fprintf(&b, "\nRecent activity on %s\n", f.Machine)
	for _, e := range f.Activity {
		fmt.Fprintf(&b, "%s %s → %s [%s] %s %s\n", fleetTime(e.Time), fleetSnippet(string(e.From)), fleetSnippet(string(e.To)), e.Kind, fleetSnippet(e.Action), fleetSnippet(e.Body))
	}
	return b.String()
}

var fleetPage = template.Must(template.New("fleet").Funcs(template.FuncMap{
	"stamp": fleetTime, "snippet": fleetSnippet, "join": func(s []string) string { return strings.Join(s, ", ") },
}).Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<meta http-equiv="refresh" content="10"><title>Agent Collaborator fleet</title>
<style>
:root{color-scheme:light dark;font:16px/1.5 system-ui,sans-serif}*{box-sizing:border-box}body{margin:auto;padding:1rem;max-width:54rem;background:#111820;color:#e6edf3;overflow-wrap:anywhere}h1,h2,h3,p{margin:.4rem 0}h1{font-size:1.6rem}h2{font-size:1.25rem;margin-top:1.3rem}h3{font-size:1.1rem}article{background:#1c2733;border:1px solid #425267;border-radius:.7rem;padding:1rem;margin:.7rem 0}small,.muted{color:#b5c3d1}.state{font-size:.9rem;color:#b9d7f5}ul{padding-left:1.2rem}li{margin:.7rem 0}time{font-size:.85rem}a{color:#add8ff}
</style></head><body>
<header><h1>Agent Collaborator fleet · {{.Machine}}</h1><p>Generated <time>{{stamp .Generated}}</time> · refreshes every 10 seconds</p><p>Conversations are in <span>Claude Remote Control</span>.</p></header>
<main><h2>Machines</h2>
{{range .Machines}}<article><h3>{{.Name}} <span class="state">{{if .Online}}online{{else}}offline{{end}}</span></h3>
{{if .Error}}<p>Inventory unavailable: {{snippet .Error}}</p>{{end}}
<p>Usable engines: {{if .Engines}}{{join .Engines}}{{else}}none reported{{end}}</p>
<ul>{{range .Sessions}}<li><strong>{{.Name}}</strong> · {{if .Agent}}{{.Agent}}{{else}}unknown agent{{end}}<br><small>{{if .Detected}}detected{{else}}registered{{end}} · last seen <time>{{stamp .Seen}}</time></small></li>{{else}}<li>No sessions reported.</li>{{end}}</ul></article>{{end}}
<h2>Recent activity on {{.Machine}}</h2><p class="muted">Last 30 local audit entries.</p>
{{range .Activity}}<article><time>{{stamp .Time}}</time><p>{{.From}} → {{.To}} · {{.Kind}} · {{.Action}}</p><p>{{snippet .Body}}</p></article>{{else}}<p>No activity recorded.</p>{{end}}
</main></body></html>`))
