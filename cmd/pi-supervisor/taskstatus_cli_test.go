package main

// ADR-0014 owner amendment: the status output carries both the structured
// task progress (JSON) and one readable line per job. Failed lookups print
// the fallback WITH its reason and human explanation — never a silent 0/0 —
// and a valid empty list reads as 0/0 with no error note.

import (
	"encoding/json"
	"strings"
	"testing"

	"pi-supervisor/internal/job"
)

func TestTaskProgressLinesSingleJob(t *testing.T) {
	st := job.Status{Name: "job1", Tasks: &job.TaskProgress{Completed: 4, Total: 7, Valid: true, StorePath: "/x/tasks.json"}}
	b, _ := json.Marshal(st)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	lines := taskProgressLines(m)
	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	if !strings.Contains(lines[0], "tasks 4/7 completed") {
		t.Fatalf("readable line: %q", lines[0])
	}
	if strings.Contains(lines[0], "NOT verified") {
		t.Fatalf("a valid read must not warn: %q", lines[0])
	}
}

func TestTaskProgressLinesFallback(t *testing.T) {
	st := job.Status{Name: "job1", Tasks: &job.TaskProgress{
		Completed: 0, Total: 0, Reason: "task-store-missing",
		Detail: "the task store for this session is missing",
	}}
	b, _ := json.Marshal(st)
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	lines := taskProgressLines(m)
	if len(lines) != 1 {
		t.Fatalf("lines = %v", lines)
	}
	if !strings.Contains(lines[0], "0/0") ||
		!strings.Contains(lines[0], "task-store-missing") ||
		!strings.Contains(lines[0], "NOT verified") {
		t.Fatalf("fallback must explain itself: %q", lines[0])
	}
}

func TestTaskProgressLinesList(t *testing.T) {
	var list []map[string]any
	for _, tp := range []job.TaskProgress{
		{Completed: 1, Total: 2, Valid: true},
		{Reason: "task-identity-unavailable", Detail: "no session identity yet"},
	} {
		st := job.Status{Name: "j-" + tp.Reason, Tasks: &tp}
		list = append(list, map[string]any{})
		b, _ := json.Marshal(st)
		_ = json.Unmarshal(b, &list[len(list)-1])
	}
	// On the wire a list decodes as []any, not []map[string]any.
	wire := make([]any, len(list))
	for i, m := range list {
		wire[i] = any(m)
	}
	lines := taskProgressLines(wire)
	if len(lines) != 2 {
		t.Fatalf("lines = %v", lines)
	}
	if !strings.Contains(lines[0], "tasks j-: 1/2 completed") {
		t.Fatalf("list line 0: %q", lines[0])
	}
	if !strings.Contains(lines[1], "task-identity-unavailable") {
		t.Fatalf("list line 1: %q", lines[1])
	}
}

func TestTaskProgressLinesNonStatusShapes(t *testing.T) {
	cases := []any{nil, "logs", []any{"a.log"}, map[string]any{"other": true}}
	for _, c := range cases {
		if lines := taskProgressLines(c); lines != nil {
			t.Fatalf("non-status payload produced lines: %v", lines)
		}
	}
}
