package node

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/TheAdaply/agent-collaborator/internal/proto"
	"github.com/TheAdaply/agent-collaborator/internal/store"
)

type fleetTransport struct {
	Transport
	login string
	fail  bool
}

func (f fleetTransport) Identify(context.Context, *http.Request) (*proto.Identity, error) {
	if f.fail {
		return nil, fmt.Errorf("unknown caller")
	}
	return &proto.Identity{Login: f.login, Machine: "phone"}, nil
}

func TestFleetOwnerOnly(t *testing.T) {
	n := testNode(t)
	setClaudeDir(t, t.TempDir())
	if err := n.st.PutSession(store.Session{Name: "worker", Agent: "codex", Deliver: "inbox", Dir: "/private/session/cwd"}); err != nil {
		t.Fatal(err)
	}
	n.st.Audit(store.AuditEntry{From: "here/worker", To: "there", Kind: proto.Tell, Body: "<script>alert(1)</script>"})
	base := n.tr
	for _, path := range []string{"/", "/fleet"} {
		for _, tc := range []struct {
			name, login string
			fail        bool
			want        int
		}{
			{"owner", "me@github", false, 200},
			{"team", "mate@github", false, 403},
			{"unknown", "stranger@github", false, 403},
			{"unidentified", "", true, 403},
			{"empty", "", false, 403},
		} {
			t.Run(path+"/"+tc.name, func(t *testing.T) {
				n.tr = fleetTransport{Transport: base, login: tc.login, fail: tc.fail}
				rec := httptest.NewRecorder()
				n.peerAPI().ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
				if rec.Code != tc.want {
					t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
				}
				body := rec.Body.String()
				if tc.want == 200 {
					for _, want := range []string{"here", "worker", "codex", "registered", "last seen", "Claude Remote Control", "&lt;script&gt;"} {
						if !strings.Contains(body, want) {
							t.Errorf("missing %q", want)
						}
					}
					if !strings.Contains(rec.Header().Get("Content-Type"), "text/html") {
						t.Error("not HTML")
					}
				} else if strings.Contains(body, "worker") || strings.Contains(body, "alert") {
					t.Error("private content leaked")
				}
				if strings.Contains(body, "/private/session/cwd") || strings.Contains(body, "<script>") {
					t.Error("unsafe content")
				}
				if rec.Header().Get("Cache-Control") != "no-store" {
					t.Error("missing no-store")
				}
			})
		}
	}
}
