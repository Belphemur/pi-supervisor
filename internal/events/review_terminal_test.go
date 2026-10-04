package events

import "testing"

// `reviewing` must NOT be terminal: the build job is done but the review
// campaign still owns the round loop, so a watch that closed on it would stop
// listening mid-campaign (ADR-0012 §4.1).
func TestReviewingIsNotTerminal(t *testing.T) {
	for _, ev := range []string{"reviewing", "review_round_done", "review_armed",
		"review_skipped", "review_gate_error", "bulk_resolve_requested",
		"bulk_resolve_applied", "bulk_resolve_expired", "review_auto_armed",
		"round_done", "backoff", "job_started"} {
		if (Event{Event: ev}).Terminal() {
			t.Errorf("%q must NOT be terminal — a watch would stop mid-campaign", ev)
		}
	}
}

// A campaign ends with review_done (clean) or review_exhausted (budget spent);
// both must close the stream, or `watch -t` hangs forever.
func TestCampaignEndEventsAreTerminal(t *testing.T) {
	for _, ev := range []string{"review_done", "review_exhausted"} {
		if !(Event{Event: ev}).Terminal() {
			t.Errorf("%q must be terminal — it is how a campaign ends", ev)
		}
	}
	// The pre-existing terminals must stay terminal.
	for _, ev := range []string{"done", "fatal", "stopped"} {
		if !(Event{Event: ev}).Terminal() {
			t.Errorf("%q regressed: must stay terminal", ev)
		}
	}
	// An unknown event must never be terminal.
	if (Event{Event: "some_new_thing"}).Terminal() {
		t.Error("an unknown event must not be terminal by default")
	}
}
