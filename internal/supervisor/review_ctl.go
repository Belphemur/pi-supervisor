package supervisor

import (
	"context"
	"encoding/json"

	"pi-supervisor/internal/control"
	"pi-supervisor/internal/review"
)

// reviewReq is the control-socket payload for the review surface
// (ADR-0012 §3). One shape serves both the campaign verbs (`review`,
// `ack`) and the shim's per-action calls, because they are the same protocol
// spoken by two clients.
type reviewReq struct {
	Cmd string `json:"cmd"` // review|review_action|ack
	// Job is the campaign's job. For review_action it names the job whose
	// LIVE round authorizes the call — the daemon refuses anything else.
	Job string `json:"job,omitempty"`
	PR  int    `json:"pr,omitempty"`
	// Rounds is the campaign budget; Type is acceptance|rebuttal.
	Rounds int    `json:"rounds,omitempty"`
	Type   string `json:"type,omitempty"`
	Auto   bool   `json:"auto,omitempty"`
	// Event is the ack id for cmd:"ack".
	Event string `json:"event,omitempty"`

	// The remaining fields are the review_action body.
	Action    string        `json:"action,omitempty"`
	ThreadID  string        `json:"thread_id,omitempty"`
	ThreadIDs []string      `json:"thread_ids,omitempty"`
	Reason    string        `json:"reason,omitempty"`
	State     string        `json:"state,omitempty"`
	Round     int           `json:"round,omitempty"`
	Replies   []reviewReply `json:"replies,omitempty"`
}

// reviewResp is the control-socket reply for a review call.
type reviewResp struct {
	OK      bool   `json:"ok"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
	Job     string `json:"job,omitempty"`
	Round   int    `json:"round,omitempty"`
	Data    any    `json:"data,omitempty"`
}

// HandleReview dispatches a review control request. It is one entry point for
// all three verbs so the CLI, the shim and tests share a single path.
//
// The payload is the RAW request body: the envelope in internal/control only
// carries routing fields, while the verbs need the full body (thread ids,
// replies, reasons). Decoding here keeps one wire shape with two views of it
// instead of two shapes that can drift.
func (s *Supervisor) HandleReview(ctx context.Context, ctlReq control.ReviewRequest) any {
	var req reviewReq
	if len(ctlReq.Raw) > 0 {
		if err := json.Unmarshal(ctlReq.Raw, &req); err != nil {
			return reviewResp{OK: false, Reason: string(review.ReasonUsage),
				Message: "bad review json: " + err.Error()}
		}
	}
	// The raw body always carries cmd/job; fall back to the envelope when a
	// caller passed an already-typed struct.
	if req.Cmd == "" {
		req.Cmd = ctlReq.Cmd
	}
	if req.Job == "" {
		req.Job = ctlReq.Job
	}
	switch req.Cmd {
	case "review":
		if req.Auto {
			data, err := s.ArmAutoReview(req.Job, req.Rounds, req.Type)
			return reviewResultResp(req.Job, data, err)
		}
		data, err := s.StartReview(ctx, req.Job, req.PR, req.Rounds, req.Type)
		return reviewResultResp(req.Job, data, err)
	case "ack":
		data, err := s.AckBulkResolve(ctx, req.Job, req.Event)
		return reviewResultResp(req.Job, data, err)
	case "review_action":
		res := s.ReviewAction(ctx, reviewAction{
			Action:    req.Action,
			Job:       req.Job,
			PR:        req.PR,
			ThreadID:  req.ThreadID,
			ThreadIDs: req.ThreadIDs,
			Reason:    req.Reason,
			State:     req.State,
			Round:     req.Round,
			Replies:   req.Replies,
		})
		if !res.OK {
			return reviewResp{
				OK: res.OK, Reason: res.Reason, Message: res.Message,
				Job: res.Job, Round: res.Round,
			}
		}
		return reviewResp{OK: true, Job: res.Job, Round: res.Round, Data: res.Payload}
	default:
		return reviewResp{OK: false, Reason: string(review.ReasonUsage),
			Message: "unknown review cmd " + req.Cmd}
	}
}

// reviewResultResp normalizes an error into the closed reason enum so the
// CLI never has to classify prose.
func reviewResultResp(job string, data any, err error) reviewResp {
	if err == nil {
		return reviewResp{OK: true, Job: job, Data: data}
	}
	return reviewResp{
		OK: false, Job: job,
		Reason:  string(asRefusal(err).Reason),
		Message: asRefusal(err).Message,
	}
}
