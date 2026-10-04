package control

import (
	"context"
	"encoding/json"
	"testing"

	"pi-supervisor/internal/job"
)

// fakeReviewer records what the review surface received and replies with a
// canned payload, so the wire encoding can be asserted without a daemon.
type fakeReviewer struct {
	got  ReviewRequest
	resp any
}

func (f *fakeReviewer) HandleReview(_ context.Context, req ReviewRequest) any {
	f.got = req
	if f.resp == nil {
		return map[string]any{"ok": true, "job": req.Job, "data": "fine"}
	}
	return f.resp
}

// fakeHandlerWithReview is the suite's existing fakeHandler plus the optional
// review surface — the same optional-interface split the daemon uses.
type fakeHandlerWithReview struct {
	*fakeReviewer
}

func (f *fakeHandlerWithReview) Status(string) (any, error) { return nil, nil }
func (f *fakeHandlerWithReview) StatusAll() any             { return nil }
func (f *fakeHandlerWithReview) Start(string) error         { return nil }
func (f *fakeHandlerWithReview) Stop(string) error          { return nil }
func (f *fakeHandlerWithReview) Steer(string, string, bool, bool) (job.SteerReport, error) {
	return job.SteerReport{}, nil
}
func (f *fakeHandlerWithReview) Logs(string, int) ([]string, error) { return nil, nil }
func (f *fakeHandlerWithReview) Reload() error                      { return nil }
func (f *fakeHandlerWithReview) Restart(string) error               { return nil }

func TestDispatchReviewRoutesToReviewer(t *testing.T) {
	fr := &fakeReviewer{}
	h := &fakeHandlerWithReview{fakeReviewer: fr}
	resp := dispatchReview(h, []byte(`{"cmd":"review","job":"j","pr":43,"rounds":5}`))
	if !resp.OK {
		t.Fatalf("dispatchReview ok = false, err = %s", resp.Error)
	}
	if fr.got.Cmd != "review" || fr.got.Job != "j" || fr.got.PR != 43 || fr.got.Rounds != 5 {
		t.Fatalf("reviewer got %+v", fr.got)
	}
}

// The raw body must reach the supervisor intact: the verb payload (thread ids,
// replies) lives there, and the envelope only carries routing fields.
func TestDispatchReviewForwardsRawBody(t *testing.T) {
	fr := &fakeReviewer{}
	h := &fakeHandlerWithReview{fakeReviewer: fr}
	body := `{"cmd":"review_action","job":"j","action":"post_replies",` +
		`"replies":[{"thread_id":"PRRT_a","type":"acceptance","body":"hi"}]}`
	resp := dispatchReview(h, []byte(body))
	if !resp.OK {
		t.Fatalf("dispatchReview failed: %s", resp.Error)
	}
	var back map[string]any
	if err := json.Unmarshal(fr.got.Raw, &back); err != nil {
		t.Fatalf("raw body is not valid JSON: %v", err)
	}
	if back["action"] != "post_replies" {
		t.Errorf("raw body lost the action: %v", back)
	}
	if _, has := back["replies"]; !has {
		t.Error("raw body lost the replies payload")
	}
}

// A refusal must surface its reason as DATA, not just as an error string:
// the consumer is an LLM that branches on the symbol (ADR-0012 §2.4).
func TestDispatchReviewSurfacesReason(t *testing.T) {
	fr := &fakeReviewer{resp: map[string]any{
		"ok": false, "reason": "not-answered-this-round",
		"message": "thread PRRT_a was not answered in round 3",
		"job":     "j",
	}}
	h := &fakeHandlerWithReview{fakeReviewer: fr}
	resp := dispatchReview(h, []byte(`{"cmd":"review_action","job":"j","action":"resolve_thread"}`))
	if resp.OK {
		t.Fatal("a refusal must not report ok")
	}
	if resp.Reason != "not-answered-this-round" {
		t.Fatalf("reason = %q, want the stable symbol", resp.Reason)
	}
	if resp.Error == "" {
		t.Error("the human-readable message must still be present")
	}
	// ok/reason/message are lifted to the envelope, not duplicated in data.
	m, _ := resp.Data.(map[string]any)
	for _, k := range []string{"ok", "reason", "message"} {
		if _, dup := m[k]; dup {
			t.Errorf("%q must not be duplicated into data", k)
		}
	}
	if m["job"] != "j" {
		t.Errorf("data lost the job field: %v", m)
	}
}

func TestDispatchReviewBadJSON(t *testing.T) {
	h := &fakeHandlerWithReview{fakeReviewer: &fakeReviewer{}}
	resp := dispatchReview(h, []byte(`{not json`))
	if resp.OK {
		t.Fatal("malformed JSON must be refused")
	}
	if resp.Reason != "usage" {
		t.Errorf("reason = %q, want usage", resp.Reason)
	}
}

// A handler that does not implement the optional review surface must degrade
// honestly rather than panic.
func TestDispatchReviewWithoutReviewer(t *testing.T) {
	resp := dispatchReview(&fakeHandler{}, []byte(`{"cmd":"review","job":"j"}`))
	if resp.OK {
		t.Fatal("a handler without the review surface must refuse")
	}
	if resp.Error != "review unsupported" {
		t.Errorf("error = %q, want the unsupported message", resp.Error)
	}
}

// Every review verb must be ROUTED, not just handled.
//
// The first cut of the post-completion re-check feature added
// review_recheck_all to the supervisor's own switch but not to the control
// layer's verb list, so the daemon answered `unknown cmd review_recheck_all`
// and the post-push hook silently did nothing on every push.
//
// Every test that missed this called the supervisor method DIRECTLY, so the
// routing layer was never exercised. This one goes through dispatch — the same
// path a socket request takes — and asserts the verb reaches the reviewer.
func TestDispatchRoutesEveryReviewVerb(t *testing.T) {
	for _, cmd := range []string{"review", "review_action", "ack", "review_recheck", "review_recheck_all"} {
		t.Run(cmd, func(t *testing.T) {
			fr := &fakeReviewer{}
			h := &fakeHandlerWithReview{fakeReviewer: fr}
			resp := dispatch(h, []byte(`{"cmd":"`+cmd+`","job":"j","pushed":true,"repo":"o/r"}`))
			if !resp.OK {
				t.Fatalf("cmd %q was not routed: %s", cmd, resp.Error)
			}
			if fr.got.Cmd != cmd {
				t.Fatalf("reviewer saw cmd %q, want %q", fr.got.Cmd, cmd)
			}
			if cmd == "review_recheck_all" {
				if !fr.got.Pushed || fr.got.Repo != "o/r" {
					t.Fatalf("repo scope lost: pushed=%v repo=%q", fr.got.Pushed, fr.got.Repo)
				}
			}
		})
	}
}
