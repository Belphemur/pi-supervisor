package main

import (
	"encoding/json"
	"strings"
	"testing"
)

// The status stdout is a machine contract: exactly ONE valid JSON document,
// jq-parseable, with pr_url carried inside it (ADR-0006). The old trailing
// `pr <url>` line broke every `| jq` pipe; this test fails if it ever comes
// back.
func TestRenderDataStatusIsSingleJSONDoc(t *testing.T) {
	out := captureStdout(t, func() {
		renderData(map[string]any{
			"name":   "power-top",
			"pr_url": "https://github.com/Belphemur/XPoint/pull/184",
		})
	})
	dec := json.NewDecoder(strings.NewReader(out))
	var v any
	if err := dec.Decode(&v); err != nil {
		t.Fatalf("status output is not valid JSON: %v\n%s", err, out)
	}
	if dec.More() {
		t.Fatalf("status output carries more than one JSON document:\n%s", out)
	}
	if strings.Contains(out, "\npr ") || strings.HasPrefix(out, "pr ") {
		t.Fatalf("status output must not carry the trailing `pr <url>` line:\n%s", out)
	}
	m, ok := v.(map[string]any)
	if !ok {
		t.Fatalf("single-job status should decode as an object, got %T", v)
	}
	if m["pr_url"] != "https://github.com/Belphemur/XPoint/pull/184" {
		t.Fatalf("pr_url lost in render: %v", m)
	}
}

// A list status (all jobs) is one JSON array, not one document per job.
func TestRenderDataStatusAllIsOneJSONArray(t *testing.T) {
	out := captureStdout(t, func() {
		renderData([]any{
			map[string]any{"name": "alpha"},
			map[string]any{"name": "beta"},
		})
	})
	var v any
	if err := json.Unmarshal([]byte(out), &v); err != nil {
		t.Fatalf("status-all output is not valid JSON: %v\n%s", err, out)
	}
	arr, ok := v.([]any)
	if !ok || len(arr) != 2 {
		t.Fatalf("status-all should be a 2-element array, got %T", v)
	}
}

// Logs keep their line-per-entry shape (not JSON): each entry is its own
// printed line.
func TestRenderDataLogsStaysLinePerEntry(t *testing.T) {
	out := captureStdout(t, func() {
		renderData([]any{"line one", "line two"})
	})
	got := strings.Split(strings.TrimRight(out, "\n"), "\n")
	if len(got) != 2 || got[0] != "line one" || got[1] != "line two" {
		t.Fatalf("logs render = %q, want two bare lines", out)
	}
}

// A nil payload (start/stop/reload success) prints NOTHING: `null` on stdout
// is noise, and scripts pipe status — never those verbs — so silence is the
// honest success output.
func TestRenderDataNilPrintsNothing(t *testing.T) {
	out := captureStdout(t, func() { renderData(nil) })
	if out != "" {
		t.Fatalf("nil payload should print nothing, got %q", out)
	}
}
