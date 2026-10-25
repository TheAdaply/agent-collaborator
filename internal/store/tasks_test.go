package store

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
)

func TestTaskTransitionsAndAtomicClaim(t *testing.T) {
	s, err := Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	claims := make(chan bool, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, created, err := s.CreateTask(Task{ID: "one"})
			if err != nil {
				t.Error(err)
			}
			claims <- created
		}()
	}
	wg.Wait()
	close(claims)
	count := 0
	for created := range claims {
		if created {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("claims: %d", count)
	}
	task, _ := s.LoadTask("one")
	if task.State != Received {
		t.Fatal(task.State)
	}
	if err := s.TransitionTask("one", Reported, "", "", ""); err == nil {
		t.Fatal("invalid transition accepted")
	}
	for _, state := range []TaskState{Running, Reported} {
		if err := s.TransitionTask("one", state, "e", "result", ""); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.TransitionTask("one", Running, "", "", ""); err == nil {
		t.Fatal("terminal task restarted")
	}
	reopened, _ := Open(s.Dir())
	task, err = reopened.LoadTask("one")
	if err != nil || task.State != Reported || task.Attempts != 1 || task.ResultSnippet != "result" {
		t.Fatalf("task: %+v %v", task, err)
	}
	if _, _, err := s.CreateTask(Task{ID: "../escape"}); err == nil {
		t.Fatal("unsafe ID accepted")
	}
	if err := os.WriteFile(filepath.Join(s.Dir(), "tasks", "broken.json"), []byte("bad"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.CreateTask(Task{ID: "broken"}); err == nil {
		t.Fatal("corrupt receipt overwritten")
	}
}
