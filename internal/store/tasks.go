package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/TheAdaply/agent-collaborator/internal/proto"
)

type TaskState string

const (
	Received    TaskState = "received"
	Running     TaskState = "running"
	Reported    TaskState = "reported"
	Failed      TaskState = "failed"
	Interrupted TaskState = "interrupted"
)

// Task is the durable receipt for an incoming ask or tell. Records are retained for deduplication.
type Task struct {
	ID            string        `json:"id"`
	Kind          proto.Kind    `json:"kind"`
	From          proto.Address `json:"from"`
	To            proto.Address `json:"to"`
	Body          string        `json:"body"`
	Trust         proto.Trust   `json:"trust"`
	CreatedAt     time.Time     `json:"created_at"`
	UpdatedAt     time.Time     `json:"updated_at"`
	State         TaskState     `json:"state"`
	Attempts      int           `json:"attempts"`
	Engine        string        `json:"engine,omitempty"`
	ResultSnippet string        `json:"result_snippet,omitempty"`
	Error         string        `json:"error,omitempty"`
}

// CreateTask atomically claims an ID. An existing receipt is returned unchanged.
func (s *Store) CreateTask(t Task) (*Task, bool, error) {
	if err := safe(t.ID); err != nil {
		return nil, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var old Task
	path := s.path("tasks", t.ID+".json")
	if err := readJSON(path, &old); err == nil {
		return &old, false, nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, false, err
	}
	t.State = Received
	t.CreatedAt = time.Now()
	t.UpdatedAt = t.CreatedAt
	t.Body = taskClip(t.Body)
	if err := writeJSON(path, t); err != nil {
		return nil, false, err
	}
	return &t, true, nil
}

func (s *Store) LoadTask(id string) (*Task, error) {
	if err := safe(id); err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var t Task
	err := readJSON(s.path("tasks", id+".json"), &t)
	return &t, err
}

// ListTasks returns newest receipts first; limit <= 0 returns all for recovery.
func (s *Store) ListTasks(limit int) ([]Task, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := filepath.Glob(s.path("tasks", "*.json"))
	if err != nil {
		return nil, err
	}
	out := []Task{}
	for _, f := range files {
		var t Task
		if err := readJSON(f, &t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// TransitionTask validates the state machine and persists completion metadata atomically.
func (s *Store) TransitionTask(id string, state TaskState, engine, result, detail string) error {
	if err := safe(id); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	path := s.path("tasks", id+".json")
	var t Task
	if err := readJSON(path, &t); err != nil {
		return err
	}
	valid := t.State == Received && (state == Running || state == Failed) || t.State == Running && (state == Reported || state == Failed || state == Interrupted)
	if !valid {
		return fmt.Errorf("task %s: invalid transition %s -> %s", id, t.State, state)
	}
	t.State = state
	t.UpdatedAt = time.Now()
	if state == Running {
		t.Attempts++
	}
	if engine != "" {
		t.Engine = engine
	}
	t.ResultSnippet = taskClip(result)
	t.Error = taskClip(detail)
	return writeJSON(path, t)
}

func taskClip(s string) string {
	r := []rune(s)
	if len(r) > 2000 {
		return string(r[:2000]) + "…"
	}
	return s
}
