package taskwatch

import (
	"path/filepath"
	"testing"
)

// ── Status counts (owner amendment): the ONE resolver adapter also feeds
// `status` counts on demand. A valid empty list is 0/0 with NO error; a
// failed lookup is 0/0 PLUS a machine-readable reason; totals describe the
// CURRENT list (auto-clear/deletion shrink them). ──

func TestCountsValidList(t *testing.T) {
	dir := t.TempDir()
	path := writeStore(t, filepath.Join(dir, "tasks-fake-sess-1.json"), `{"nextId": 4, "tasks": [
	 {"id":"1","subject":"a","status":"completed","blocks":[],"blockedBy":[],"createdAt":1,"updatedAt":1},
	 {"id":"2","subject":"b","status":"in_progress","blocks":[],"blockedBy":[],"createdAt":1,"updatedAt":1},
	 {"id":"3","subject":"c","status":"completed","blocks":[],"blockedBy":[],"createdAt":1,"updatedAt":1},
	 {"id":"4","subject":"d","status":"pending","blocks":[],"blockedBy":[],"createdAt":1,"updatedAt":1}]}`)
	c, tot, why := Counts(path)
	if why != "" {
		t.Fatalf("unexpected failure: %s", why)
	}
	if c != 2 || tot != 4 {
		t.Fatalf("counts = %d/%d, want 2/4", c, tot)
	}
}

func TestCountsEmptyListIsZeroZeroWithoutError(t *testing.T) {
	dir := t.TempDir()
	path := writeStore(t, filepath.Join(dir, "tasks-fake-sess-1.json"),
		`{"nextId":1,"tasks":[]}`)
	c, tot, why := Counts(path)
	if why != "" {
		t.Fatalf("a valid empty list must carry no error, got %q", why)
	}
	if c != 0 || tot != 0 {
		t.Fatalf("counts = %d/%d, want 0/0", c, tot)
	}
}

func TestCountsFailuresAreZeroZeroWithError(t *testing.T) {
	dir := t.TempDir()
	cases := []struct {
		name, json string
	}{
		{"missing file", ""},
		{"bad json", "{nope"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, tc.name+".json")
			if tc.json != "" {
				writeStore(t, path, tc.json)
			}
			c, tot, why := Counts(path)
			if c != 0 || tot != 0 {
				t.Fatalf("fallback must be 0/0, got %d/%d", c, tot)
			}
			if why == "" {
				t.Fatal("failed lookup needs a machine-readable reason")
			}
		})
	}
}

// Resolver-level Counts: identity resolves the LIVE session store; a missing
// session id fails the lookup rather than guessing a workspace file.
func TestResolverCountsIdentity(t *testing.T) {
	fixture := t.TempDir()
	cwd := t.TempDir()
	r := Resolver{Env: map[string]string{}, AgentDir: fixture, Cwd: cwd, SessionID: "sess-9"}
	if _, _, why := r.Counts(); why == "" {
		t.Fatal("no store yet: lookup must fail, not guess")
	}
	// Once pi's plugin wrote the store, the same resolver counts it.
	writeStore(t, filepath.Join(cwd, ".pi", "tasks", "tasks-sess-9.json"),
		`{"nextId":2,"tasks":[{"id":"1","subject":"s","status":"completed","blocks":[],"blockedBy":[],"createdAt":1,"updatedAt":1}]}`)
	c, tot, why := r.Counts()
	if why != "" || c != 1 || tot != 1 {
		t.Fatalf("counts = %d/%d why=%q", c, tot, why)
	}
}
