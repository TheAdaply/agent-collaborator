// Package store keeps a node's durable state as plain files: one JSON file per message, written
// atomically, so a crash never corrupts anything and everything can be inspected with ls and cat.
//
//	inbox/<session>/<id>.json   messages waiting for a session that pulls them (MCP check_inbox)
//	pending/<id>.json           asks this node received and still owes a reply to
//	replies/<id>.json           replies to asks this node sent (what `agent-collaborator ask` waits for)
//	asked/<id>.json             asks this node sent: which machine may answer them
//	outbox/<id>.json            messages to peers that were unreachable, retried until they expire
//	sessions.json               local sessions and how to deliver to them
//	audit.jsonl                 every message in and out, and what was done with it
package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/TheAdaply/agent-collaborator/internal/proto"
)

// Store is a node's state directory.
type Store struct {
	dir string
	mu  sync.Mutex
}

// Open creates the layout under dir.
func Open(dir string) (*Store, error) {
	for _, d := range []string{"inbox", "pending", "replies", "outbox", "asked"} {
		if err := os.MkdirAll(filepath.Join(dir, d), 0o700); err != nil {
			return nil, err
		}
	}
	return &Store{dir: dir}, nil
}

// Session is a local agent session that can receive messages.
type Session struct {
	Name string `json:"name"`
	// Deliver is how a message reaches the session:
	//   inbox                   the session pulls it (MCP check_inbox / Agent Collaborator inbox)
	//   claude:<id-or-name>     posted to a Claude Code session's socket (wakes an idle turn)
	//   tmux:<socket>:<target>  typed into that terminal pane
	//   router                  handed to the engine router, like a bare machine address
	Deliver string `json:"deliver"`
	// Fallback is a tmux spec used when claude: push fails (optional).
	Fallback string `json:"fallback,omitempty"`
	// Team: teammates may message this session. Off by default: only your own machines can.
	Team  bool   `json:"team"`
	Agent string `json:"agent,omitempty"` // claude, codex, ... (informational)
	// PID of the process that registered the session (the MCP server): when it is gone, so is the session.
	PID int `json:"pid,omitempty"`
	// Detected marks a session found live on the machine but never registered (owner view only).
	Detected bool      `json:"detected,omitempty"`
	Dir      string    `json:"dir,omitempty"`
	Seen     time.Time `json:"seen"` // last registration or inbox poll
}

// Pending is an ask that this node received and must answer.
type Pending struct {
	Msg      proto.Message `json:"msg"`
	Received time.Time     `json:"received"`
}

// Outgoing is a message waiting for its peer to come online.
type Outgoing struct {
	Msg      proto.Message `json:"msg"`
	Queued   time.Time     `json:"queued"`
	Attempts int           `json:"attempts"`
	LastErr  string        `json:"last_err,omitempty"`
}

// AuditEntry is one line of audit.jsonl.
type AuditEntry struct {
	Time   time.Time     `json:"time"`
	Dir    string        `json:"dir"` // in | out
	ID     string        `json:"id"`
	Kind   proto.Kind    `json:"kind"`
	From   proto.Address `json:"from"`
	To     proto.Address `json:"to"`
	Trust  proto.Trust   `json:"trust,omitempty"`
	Action string        `json:"action"` // delivered, queued, refused, routed:<engine>, ...
	Detail string        `json:"detail,omitempty"`
	Body   string        `json:"body,omitempty"` // the message text (truncated), for the chats view
}

// Dir returns the state directory.
func (s *Store) Dir() string { return s.dir }

func (s *Store) path(parts ...string) string {
	return filepath.Join(append([]string{s.dir}, parts...)...)
}

func writeJSON(path string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// safe rejects path components that could escape the state directory.
func safe(name string) error {
	if name == "" || name == "." || name == ".." || strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("bad name %q", name)
	}
	return nil
}

// --- sessions ---

// Sessions returns the registered sessions by name.
func (s *Store) Sessions() (map[string]Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.sessionsLocked()
}

func (s *Store) sessionsLocked() (map[string]Session, error) {
	m := map[string]Session{}
	if err := readJSON(s.path("sessions.json"), &m); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	return m, nil
}

// PutSession registers or updates a session.
func (s *Store) PutSession(ss Session) error {
	if err := safe(ss.Name); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.sessionsLocked()
	if err != nil {
		return err
	}
	ss.Seen = time.Now()
	m[ss.Name] = ss
	return writeJSON(s.path("sessions.json"), m)
}

// Prune removes sessions for which gone reports true, and returns their names.
func (s *Store) Prune(gone func(Session) bool) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.sessionsLocked()
	if err != nil {
		return nil
	}
	var removed []string
	for name, ss := range m {
		if gone(ss) {
			delete(m, name)
			removed = append(removed, name)
		}
	}
	if len(removed) > 0 {
		_ = writeJSON(s.path("sessions.json"), m)
	}
	return removed
}

// RemoveSession unregisters a session.
func (s *Store) RemoveSession(name string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	m, err := s.sessionsLocked()
	if err != nil {
		return err
	}
	delete(m, name)
	return writeJSON(s.path("sessions.json"), m)
}

// TouchSession marks a session as recently seen.
func (s *Store) TouchSession(name string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if m, err := s.sessionsLocked(); err == nil {
		if ss, ok := m[name]; ok {
			ss.Seen = time.Now()
			m[name] = ss
			_ = writeJSON(s.path("sessions.json"), m)
		}
	}
}

// --- inbox ---

// PutInbox stores a message for a session that pulls.
func (s *Store) PutInbox(session string, m *proto.Message) error {
	if err := safe(session); err != nil {
		return err
	}
	return writeJSON(s.path("inbox", session, m.ID+".json"), m)
}

// TakeInbox returns and removes a session's waiting messages, oldest first.
func (s *Store) TakeInbox(session string) ([]proto.Message, error) {
	if err := safe(session); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	files, _ := filepath.Glob(s.path("inbox", session, "*.json"))
	sort.Strings(files) // ids start with a timestamp
	var out []proto.Message
	for _, f := range files {
		var m proto.Message
		if err := readJSON(f, &m); err == nil {
			out = append(out, m)
		}
		_ = os.Remove(f)
	}
	return out, nil
}

// --- pending asks ---

// PutPending records an ask that needs a reply.
func (s *Store) PutPending(m *proto.Message) error {
	return writeJSON(s.path("pending", m.ID+".json"), Pending{Msg: *m, Received: time.Now()})
}

// TakePending returns and removes a pending ask, so each ask is answered once.
func (s *Store) TakePending(id string) (*Pending, error) {
	if err := safe(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var p Pending
	f := s.path("pending", id+".json")
	if err := readJSON(f, &p); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, fmt.Errorf("no pending ask %s (already answered, or not addressed to this machine)", id)
		}
		return nil, err
	}
	return &p, os.Remove(f)
}

// --- replies ---

// PutReply stores the reply to an ask this node sent.
func (s *Store) PutReply(m *proto.Message) error {
	if err := safe(m.ReplyTo); err != nil {
		return err
	}
	return writeJSON(s.path("replies", m.ReplyTo+".json"), m)
}

// Reply returns the reply to ask id, if it has arrived.
func (s *Store) Reply(id string) (*proto.Message, bool) {
	if safe(id) != nil {
		return nil, false
	}
	var m proto.Message
	if err := readJSON(s.path("replies", id+".json"), &m); err != nil {
		return nil, false
	}
	return &m, true
}

// --- asks sent ---

// Asked records that this node sent ask id to machine, so only that machine's reply is accepted.
func (s *Store) Asked(id, machine string) error {
	return writeJSON(s.path("asked", id+".json"), map[string]string{"machine": machine})
}

// AskedOf returns the machine ask id was sent to.
func (s *Store) AskedOf(id string) (string, bool) {
	if safe(id) != nil {
		return "", false
	}
	var m map[string]string
	if err := readJSON(s.path("asked", id+".json"), &m); err != nil {
		return "", false
	}
	return m["machine"], true
}

// --- outbox ---

// Queue stores a message for later delivery.
func (s *Store) Queue(o *Outgoing) error {
	return writeJSON(s.path("outbox", o.Msg.ID+".json"), o)
}

// Outbox returns queued messages, oldest first.
func (s *Store) Outbox() []Outgoing {
	files, _ := filepath.Glob(s.path("outbox", "*.json"))
	sort.Strings(files)
	var out []Outgoing
	for _, f := range files {
		var o Outgoing
		if readJSON(f, &o) == nil {
			out = append(out, o)
		}
	}
	return out
}

// Dequeue removes a delivered or expired message from the outbox.
func (s *Store) Dequeue(id string) { _ = os.Remove(s.path("outbox", id+".json")) }

// --- audit ---

// Audit appends one entry to audit.jsonl.
func (s *Store) Audit(e AuditEntry) {
	e.Time = time.Now()
	b, _ := json.Marshal(e)
	s.mu.Lock()
	defer s.mu.Unlock()
	f, err := os.OpenFile(s.path("audit.jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// AuditTail returns the last n audit entries.
func (s *Store) AuditTail(n int) []AuditEntry {
	b, err := os.ReadFile(s.path("audit.jsonl"))
	if err != nil {
		return nil
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	var out []AuditEntry
	for _, l := range lines {
		var e AuditEntry
		if json.Unmarshal([]byte(l), &e) == nil {
			out = append(out, e)
		}
	}
	return out
}
