package main

import "testing"

// The trailing `pr <url>` field of `pi-supervisor status <job>` (ADR-0006).
// Table-driven: the JSON already carries pr_url; this is the one-liner the
// operator greps for.
func TestPRStatusLine(t *testing.T) {
	tests := []struct {
		name string
		data any
		want string
	}{
		{
			name: "PR linked",
			data: map[string]any{"name": "power-top", "pr_url": "https://github.com/Belphemur/XPoint/pull/184"},
			want: "pr https://github.com/Belphemur/XPoint/pull/184",
		},
		{
			name: "no PR linked",
			data: map[string]any{"name": "power-top"},
			want: "",
		},
		{
			name: "empty pr_url",
			data: map[string]any{"name": "power-top", "pr_url": ""},
			want: "",
		},
		{
			name: "status of all jobs is a list, never a pr line",
			data: []any{map[string]any{"name": "power-top"}},
			want: "",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := prStatusLine(tc.data); got != tc.want {
				t.Fatalf("prStatusLine = %q, want %q", got, tc.want)
			}
		})
	}
}
