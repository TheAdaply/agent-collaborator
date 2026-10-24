package node

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/TheAdaply/agent-collaborator/internal/deliver"
)

func setClaudeDir(t *testing.T, dir string) {
	t.Helper()
	t.Setenv("CLAUDE_CONFIG_DIR", dir)
	deliver.SetClaudeSessionsDirForTest(dir)
	t.Cleanup(func() { deliver.SetClaudeSessionsDirForTest("") })
}

func writeClaudeSession(t *testing.T, dir, name, id, sock string) {
	t.Helper()
	b, _ := json.Marshal(deliver.ClaudeSession{Name: name, SessionID: id, SocketPath: sock, UpdatedAt: 1})
	if err := os.WriteFile(filepath.Join(dir, name+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}
