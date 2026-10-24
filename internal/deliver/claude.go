package deliver

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/TheAdaply/agent-collaborator/internal/proto"
)

// ClaudeSession is one live Claude Code session, from ~/.claude/sessions/<pid>.json. Claude Code
// writes a file per running session with the socket its messages are posted to.
type ClaudeSession struct {
	PID        int    `json:"pid"`
	SessionID  string `json:"sessionId"`
	CWD        string `json:"cwd"`
	Name       string `json:"name"`
	Status     string `json:"status"` // busy | idle
	SocketPath string `json:"messagingSocketPath"`
	UpdatedAt  int64  `json:"updatedAt"`
}

// claudeSessionsDir is where Claude Code records its live sessions (override for tests).
var claudeSessionsDir = defaultClaudeSessionsDir

func defaultClaudeSessionsDir() string {
	if d := os.Getenv("CLAUDE_CONFIG_DIR"); d != "" {
		return filepath.Join(d, "sessions")
	}
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".claude", "sessions")
}

// ScanClaude returns the live Claude Code sessions, newest first.
func ScanClaude() []ClaudeSession {
	files, _ := filepath.Glob(filepath.Join(claudeSessionsDir(), "*.json"))
	var out []ClaudeSession
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		var s ClaudeSession
		if json.Unmarshal(b, &s) == nil && s.SocketPath != "" {
			out = append(out, s)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt > out[j].UpdatedAt })
	return out
}

// FindClaude locates a live Claude Code session by its id or its name (id wins).
func FindClaude(ref string) (ClaudeSession, bool) {
	var byName *ClaudeSession
	for _, s := range ScanClaude() {
		if s.SessionID == ref {
			return s, true
		}
		if s.Name == ref && byName == nil {
			cp := s
			byName = &cp
		}
	}
	if byName != nil {
		return *byName, true
	}
	return ClaudeSession{}, false
}

// Claude posts a message straight into a Claude Code session's inbox, which delivers it mid-turn and
// wakes an idle session. ref is a "claude:<session-id-or-name>" spec's payload.
func Claude(m *proto.Message, ref string) error {
	s, ok := FindClaude(ref)
	if !ok {
		return fmt.Errorf("no live Claude Code session %q", ref)
	}
	return postClaude(s.SocketPath, proto.Frame(m))
}

// postClaude writes one user message to a Claude Code messaging socket.
func postClaude(socket, text string) error {
	c, err := net.DialTimeout("unix", socket, 3*time.Second)
	if err != nil {
		return fmt.Errorf("connect %s: %w", socket, err)
	}
	defer c.Close()
	_ = c.SetDeadline(time.Now().Add(5 * time.Second))
	frame := map[string]any{"type": "user", "message": map[string]any{"role": "user", "content": text}}
	b, _ := json.Marshal(frame)
	if _, err := c.Write(append(b, '\n')); err != nil {
		return fmt.Errorf("write %s: %w", socket, err)
	}
	return nil
}

// ClaudeRef parses a "claude:<ref>" spec, returning the session id or name.
func ClaudeRef(spec string) (string, bool) {
	return strings.CutPrefix(spec, "claude:")
}

// SetClaudeSessionsDirForTest overrides the sessions dir (tests only). "" restores the default.
func SetClaudeSessionsDirForTest(dir string) {
	if dir == "" {
		claudeSessionsDir = defaultClaudeSessionsDir
		return
	}
	claudeSessionsDir = func() string { return dir }
}
