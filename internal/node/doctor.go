package node

import (
	"net/http"
)

// NetworkHealth is a read-only snapshot of the daemon's existing transport.
type NetworkHealth struct {
	Machine string `json:"machine"`
	Joined  bool   `json:"joined"`
	Peers   int    `json:"peers"`
	Error   string `json:"error,omitempty"`
}

func (n *Node) ctlDoctorNetwork(w http.ResponseWriter, r *http.Request) {
	health := NetworkHealth{Machine: n.cfg.Name}
	if tr, ok := n.tr.(*tailnet); ok {
		st, err := tr.lc.StatusWithoutPeers(r.Context())
		if err != nil {
			health.Error = err.Error()
			writeJSON(w, health)
			return
		}
		if st.BackendState != "Running" || len(st.TailscaleIPs) == 0 {
			health.Error = "Tailscale is " + st.BackendState
			writeJSON(w, health)
			return
		}
	}
	if _, err := n.tr.Login(r.Context()); err != nil {
		health.Error = err.Error()
	} else {
		health.Joined = true
		peers, err := n.tr.Peers(r.Context())
		if err != nil {
			health.Error = err.Error()
		} else {
			health.Peers = len(peers)
		}
	}
	writeJSON(w, health)
}
