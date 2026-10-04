package supervisor

import (
	"testing"

	"pi-supervisor/internal/fault"
)

// FAILS ON CURRENT CODE — left failing on purpose as evidence for issue #1's
// acceptance criterion "A refused control request (e.g. `pi-supervisor status
// nope`) appears in the journal with a machine-readable reason".
//
// `pi-supervisor status nope` is the example the issue names, and it is the
// one refusal path that still returns a BARE error, so logResponse falls back
// to the catch-all and the journal records the useless
// `reason=refused` instead of `reason=unknown_job`:
//
//	WARN control event=request_refused cmd=status job=nope reason=refused err="unknown job \"nope\""
//
// Verified against the installed a9739a6 daemon. Start/Stop/Restart already
// wrap with fault.New; Status (and Steer's unknown-job branch) do not.
func TestStatusRefusalCarriesUnknownJob(t *testing.T) {
	s := New()
	_, err := s.Status("nope")
	if err == nil {
		t.Fatal("Status of an unknown job must refuse")
	}
	if got := fault.KindOf(err); got != fault.KindUnknownJob {
		t.Fatalf("reason for `status nope` = %q, want %q", got, fault.KindUnknownJob)
	}
}
