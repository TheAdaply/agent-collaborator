package node

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/TheAdaply/agent-collaborator/internal/proto"
	"github.com/TheAdaply/agent-collaborator/internal/store"
)

func TestTaskDedupRunsOnceAndReplaysReply(t *testing.T) {
	n := testNode(t)
	counter := filepath.Join(t.TempDir(), "executions")
	t.Setenv("TASK_COUNTER", counter)
	// Replace the test engine with an execution counter.
	bin := filepath.Join(t.TempDir(), "fake-e")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\necho run >> \"$TASK_COUNTER\"\necho result\n"), 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", filepath.Dir(bin)+":"+os.Getenv("PATH"))
	m := msg(proto.Ask, "here", proto.Owner, "me@github")
	m.From = "here/boss"
	var deliveries sync.WaitGroup
	for i := 0; i < 10; i++ {
		deliveries.Add(1)
		go func() {
			defer deliveries.Done()
			r := n.accept(context.Background(), m)
			if r.State != "routing" && r.State != "delivered" {
				t.Errorf("accept: %+v", r)
			}
		}()
	}
	deliveries.Wait()
	n.wg.Wait()
	task, err := n.st.LoadTask(m.ID)
	if err != nil || task.State != store.Reported || task.Attempts != 1 || task.Engine != "e" {
		t.Fatalf("task: %+v %v", task, err)
	}
	if _, ok := n.st.Reply(m.ID); !ok {
		t.Fatal("missing first reply")
	}
	if err := os.Remove(filepath.Join(n.st.Dir(), "replies", m.ID+".json")); err != nil {
		t.Fatal(err)
	}
	n.accept(context.Background(), m)
	if reply, ok := n.st.Reply(m.ID); !ok || reply.Body != task.ResultSnippet {
		t.Fatalf("missing replay: %+v", reply)
	}
	data, err := os.ReadFile(counter)
	if err != nil || string(data) != "run\n" {
		t.Fatalf("executions: %q %v", data, err)
	}
}

func TestTaskRestartInterruptsRunning(t *testing.T) {
	n := testNode(t)
	for _, id := range []string{"running-task", "received-task"} {
		if _, _, err := n.st.CreateTask(store.Task{ID: id}); err != nil {
			t.Fatal(err)
		}
	}
	if err := n.st.TransitionTask("running-task", store.Running, "", "", ""); err != nil {
		t.Fatal(err)
	}
	restarted, err := New(n.cfg)
	if err != nil {
		t.Fatal(err)
	}
	task, err := restarted.st.LoadTask("running-task")
	if err != nil || task.State != store.Interrupted || task.Error == "" {
		t.Fatalf("task: %+v %v", task, err)
	}
	received, _ := restarted.st.LoadTask("received-task")
	if received.State != store.Received {
		t.Fatalf("received changed: %+v", received)
	}
}

func TestTaskSessionLifecycleAndTellDedup(t *testing.T) {
	n := testNode(t)
	if err := n.st.PutSession(store.Session{Name: "worker", Deliver: "inbox"}); err != nil {
		t.Fatal(err)
	}
	for _, kind := range []proto.Kind{proto.Ask, proto.Tell} {
		m := msg(kind, "here/worker", proto.Owner, "me@github")
		m.From = "here/boss"
		if r := n.accept(context.Background(), m); r.State != "delivered" {
			t.Fatal(r)
		}
		task, _ := n.st.LoadTask(m.ID)
		if kind == proto.Ask {
			if task.State != store.Running {
				t.Fatal(task.State)
			}
			if _, err := n.reply(context.Background(), m.ID, "answer"); err != nil {
				t.Fatal(err)
			}
		}
		n.accept(context.Background(), m)
		inbox, err := n.st.TakeInbox("worker")
		if err != nil || len(inbox) != 1 {
			t.Fatalf("inbox: %+v %v", inbox, err)
		}
		task, _ = n.st.LoadTask(m.ID)
		if task.State != store.Reported {
			t.Fatal(task.State)
		}
	}
	recorder := httptest.NewRecorder()
	n.controlAPI().ServeHTTP(recorder, httptest.NewRequest("GET", "/v1/tasks?n=1", nil))
	var tasks []store.Task
	if err := json.Unmarshal(recorder.Body.Bytes(), &tasks); err != nil || len(tasks) != 1 {
		t.Fatalf("endpoint: %s %v", recorder.Body, err)
	}
}

func TestRoutedTellCompletes(t *testing.T) {
	n := testNode(t)
	m := msg(proto.Tell, "here", proto.Owner, "me@github")
	if r := n.accept(context.Background(), m); r.State != "routing" {
		t.Fatal(r)
	}
	n.wg.Wait()
	task, err := n.st.LoadTask(m.ID)
	if err != nil || task.State != store.Reported || task.Engine != "e" || task.ResultSnippet == "" {
		t.Fatalf("task: %+v %v", task, err)
	}
}

func TestRunningDuplicateDoesNotDispatch(t *testing.T) {
	n := testNode(t)
	if err := n.st.PutSession(store.Session{Name: "worker", Deliver: "inbox"}); err != nil {
		t.Fatal(err)
	}
	m := msg(proto.Ask, "here/worker", proto.Owner, "me@github")
	m.From = "here/boss"
	n.accept(context.Background(), m)
	task, _ := n.st.LoadTask(m.ID)
	if task.State != store.Running {
		t.Fatal(task.State)
	}
	if r := n.accept(context.Background(), m); r.State != "delivered" {
		t.Fatal(r)
	}
	messages, err := n.st.TakeInbox("worker")
	if err != nil || len(messages) != 1 {
		t.Fatalf("duplicate dispatched: %+v %v", messages, err)
	}
	if _, err := n.reply(context.Background(), m.ID, "done"); err != nil {
		t.Fatal(err)
	}
	forged := *m
	forged.From = "here/other"
	if r := n.accept(context.Background(), &forged); r.State != "refused" {
		t.Fatalf("receipt disclosed: %+v", r)
	}
}
