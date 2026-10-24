package deliver

import (
	"bufio"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/TheAdaply/agent-collaborator/internal/proto"
)

func writeSession(t *testing.T, dir string, s ClaudeSession) {
	t.Helper()
	b, _ := json.Marshal(s)
	if err := os.WriteFile(filepath.Join(dir, s.Name+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestFindClaudePrefersIDThenName(t *testing.T) {
	dir := t.TempDir()
	claudeSessionsDir = func() string { return dir }
	writeSession(t, dir, ClaudeSession{SessionID: "id-1", Name: "worker", SocketPath: "/s/a", UpdatedAt: 1})
	writeSession(t, dir, ClaudeSession{SessionID: "id-2", Name: "boss", SocketPath: "/s/b", UpdatedAt: 2})
	if s, ok := FindClaude("id-1"); !ok || s.SocketPath != "/s/a" {
		t.Fatalf("by id: %+v %v", s, ok)
	}
	if s, ok := FindClaude("boss"); !ok || s.SessionID != "id-2" {
		t.Fatalf("by name: %+v %v", s, ok)
	}
	if _, ok := FindClaude("ghost"); ok {
		t.Fatal("found a session that does not exist")
	}
}

func TestClaudePostDeliversUserFrame(t *testing.T) {
	dir := t.TempDir()
	claudeSessionsDir = func() string { return dir }
	sock := filepath.Join(dir, "msg.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	got := make(chan string, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		line, _ := bufio.NewReader(c).ReadString('\n')
		got <- line
	}()
	writeSession(t, dir, ClaudeSession{SessionID: "id-1", Name: "worker", SocketPath: sock, UpdatedAt: 1})
	m := &proto.Message{ID: "m1", Kind: proto.Tell, From: "alpha", To: "here/worker", Body: "ping"}
	if err := Claude(m, "worker"); err != nil {
		t.Fatal(err)
	}
	var frame struct {
		Type    string `json:"type"`
		Message struct {
			Role, Content string
		} `json:"message"`
	}
	if err := json.Unmarshal([]byte(<-got), &frame); err != nil {
		t.Fatal(err)
	}
	if frame.Type != "user" || frame.Message.Role != "user" {
		t.Fatalf("wrong frame: %+v", frame)
	}
	if !contains(frame.Message.Content, "agent-collaborator-message") || !contains(frame.Message.Content, "ping") {
		t.Fatalf("message body not framed as peer input: %q", frame.Message.Content)
	}
}

func TestClaudeNoSession(t *testing.T) {
	claudeSessionsDir = func() string { return t.TempDir() }
	if err := Claude(&proto.Message{ID: "m"}, "nobody"); err == nil {
		t.Fatal("expected an error for a missing session")
	}
}

func contains(s, sub string) bool { return len(s) >= len(sub) && (indexOf(s, sub) >= 0) }

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
