package fault

import (
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"strings"
	"testing"
)

// The log's `reason=` must come from the code, never from matching prose, and
// an untagged error must degrade to the catch-all rather than a guess.
func TestKindOf(t *testing.T) {
	base := errors.New("unknown job \"x\"")
	tagged := New(KindUnknownJob, base)

	if got := KindOf(tagged); got != KindUnknownJob {
		t.Fatalf("KindOf(tagged) = %q", got)
	}
	// Wrapping must not lose the kind: supervisors add context with %w.
	if got := KindOf(fmt.Errorf("restart: %w", tagged)); got != KindUnknownJob {
		t.Fatalf("KindOf(wrapped) = %q", got)
	}
	if got := KindOf(base); got != KindRefused {
		t.Fatalf("KindOf(untagged) = %q, want %q", got, KindRefused)
	}
	if got := KindOf(nil); got != "" {
		t.Fatalf("KindOf(nil) = %q, want empty", got)
	}
}

// The CLI keys on Error text and its exit codes are a contract (ADR-0008), so
// tagging an error must not change one character of its message.
func TestNewKeepsMessageAndUnwraps(t *testing.T) {
	base := errors.New(`job "x" already running`)
	e := New(KindAlreadyRunning, base)

	if e.Error() != base.Error() {
		t.Fatalf("Error() = %q, want %q", e.Error(), base.Error())
	}
	if !errors.Is(e, base) {
		t.Fatal("errors.Is must see through the tag")
	}
	if New(KindRefused, nil) != nil {
		t.Fatal("New(nil) must stay nil so callers can wrap unconditionally")
	}
}

// declaredKind is one Kind constant as the SOURCE declares it: the Go
// identifier and its string value.
type declaredKind struct {
	name  string
	value string
}

// declaredKinds parses fault.go and returns every Kind constant the package
// DECLARES, independent of All()'s hand-maintained inventory. The declared
// set is the ground truth: a new Kind added to neither All() nor exitCodes
// is still caught here, which is the whole point — the old test compared the
// two manual lists against each other and both could omit the same Kind.
func declaredKinds(t *testing.T) []declaredKind {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "fault.go", nil, 0)
	if err != nil {
		t.Fatalf("parse fault.go: %v", err)
	}
	var kinds []declaredKind
	for _, d := range file.Decls {
		gd, ok := d.(*ast.GenDecl)
		if !ok || gd.Tok != token.CONST {
			continue
		}
		// A const group may carry its type on the first spec only; later
		// specs inherit it. Track the last seen type name across the group.
		lastType := ""
		for _, spec := range gd.Specs {
			vs, ok := spec.(*ast.ValueSpec)
			if !ok {
				continue
			}
			typ := ""
			if vs.Type != nil {
				if id, ok := vs.Type.(*ast.Ident); ok {
					typ = id.Name
				}
			}
			if typ == "" {
				typ = lastType
			}
			lastType = typ
			if typ != "Kind" {
				continue
			}
			for i, name := range vs.Names {
				value := ""
				if i < len(vs.Values) {
					if lit, ok := vs.Values[i].(*ast.BasicLit); ok && lit.Kind == token.STRING {
						value, _ = strconv.Unquote(lit.Value)
					}
				}
				if value == "" {
					t.Fatalf("Kind constant %s has no string literal value; the vocabulary is a literal enum, not iota-derived", name.Name)
				}
				kinds = append(kinds, declaredKind{name: name.Name, value: value})
			}
		}
	}
	if len(kinds) == 0 {
		t.Fatal("parsed fault.go but found no Kind constants — the parser drifted from the source layout")
	}
	return kinds
}

// The exit-code table must cover EVERY kind the package declares — not merely
// every kind All() remembered to list. Both used to be hand-maintained, so a
// Kind added to neither passed the test while ExitCode()'s fallback silently
// reclassified its usage errors as retryable (exit 1 instead of the required
// 2, ADR-0008). The declared-constant set is parsed from the source, so the
// only way to add a Kind without an exit code is to also break this test.
func TestEveryKindHasAnExitCode(t *testing.T) {
	declared := declaredKinds(t)
	byValue := make(map[string]string, len(declared))
	for _, k := range declared {
		byValue[k.value] = k.name
	}

	// Every DECLARED Kind must be in the table with a valid exit code.
	for _, k := range declared {
		kind := Kind(k.value)
		if _, ok := exitCodes[kind]; !ok {
			t.Errorf("declared %s (value %q) has no exit-code mapping — ExitCode() would fall back to 1", k.name, k.value)
			continue
		}
		if code := kind.ExitCode(); code != 1 && code != 2 {
			t.Errorf("kind %q exit code = %d, want 1 or 2 (ADR-0008)", k.value, code)
		}
	}

	// All() must agree with the declared set exactly: no Kind forgotten, and
	// no entry in All() that is not a declared constant.
	all := All()
	if len(all) != len(declared) {
		t.Fatalf("All() has %d kinds, fault.go declares %d — one was added without the other", len(all), len(declared))
	}
	for _, k := range all {
		name, ok := byValue[string(k)]
		if !ok {
			t.Errorf("All() lists %q, which fault.go does not declare", string(k))
			continue
		}
		if _, ok := exitCodes[k]; !ok {
			t.Errorf("kind %q (declared as %s) has no exit-code mapping", string(k), name)
		}
	}
}

// The values are a published contract (ADR-0012 §2.4), so pin them: a shim
// branches on these exact strings.
func TestKindValuesAreStable(t *testing.T) {
	want := map[string]Kind{
		"unknown_job":             KindUnknownJob,
		"not_running":             KindNotRunning,
		"already_running":         KindAlreadyRunning,
		"already_done":            KindAlreadyDone,
		"bad_request":             KindBadRequest,
		"bad_json":                KindBadJSON,
		"unknown_cmd":             KindUnknownCmd,
		"unsupported":             KindUnsupported,
		"refused":                 KindRefused,
		"no-live-round":           KindNoLiveRound,
		"round-mismatch":          KindRoundMismatch,
		"not-answered-this-round": KindNotAnswered,
		"unknown-thread":          KindUnknownThread,
		"auth-unavailable":        KindAuthUnavailable,
		"rate-limited":            KindRateLimited,
		"github-error":            KindGitHubError,
		"usage":                   KindUsage,
	}
	for value, kind := range want {
		if string(kind) != value {
			t.Errorf("kind %q changed value to %q — the reason strings are a wire contract", value, kind)
		}
	}
}

// ADR-0013 follow-up 1: two closed enums for one wire field meant two
// spellings of the same condition, and a shim matching one missed the other
// (`no_live_round` on the steer path vs `no-live-round` on the review path).
// This test FAILS if anyone re-adds a kind whose value is the underscore or
// hyphen twin of a value already in the vocabulary.
func TestNoDuplicateSpellings(t *testing.T) {
	seen := make(map[Kind]bool, len(All()))
	for _, k := range All() {
		if seen[k] {
			t.Fatalf("kind %q appears twice in All()", k)
		}
		seen[k] = true
	}
	twins := func(s string) string {
		out := []byte(s)
		for i, c := range out {
			switch c {
			case '_':
				out[i] = '-'
			case '-':
				out[i] = '_'
			}
		}
		return string(out)
	}
	for _, k := range All() {
		s := string(k)
		if !strings.ContainsAny(s, "-_") {
			continue // a single word is its own twin; nothing to collide with
		}
		if twin := twins(s); seen[Kind(twin)] {
			t.Errorf("kind %q duplicates %q — one condition, one spelling on the wire", k, twin)
		}
	}
}

// `no_live_round` was the steer path's spelling of a condition review had
// already published as `no-live-round`. It must stay retired: KindOf must
// never hand back a spelling no client matches.
func TestLegacyNoLiveRoundSpellingIsRetired(t *testing.T) {
	const legacy Kind = "no_live_round"
	for _, k := range All() {
		if k == legacy {
			t.Fatalf("legacy spelling %q is back in the vocabulary", legacy)
		}
	}
	if got := KindOf(New(KindNoLiveRound, errors.New("no round is reading the ctrl file"))); got != KindNoLiveRound {
		t.Fatalf("KindOf = %q, want %q", got, KindNoLiveRound)
	}
}

// Every reason that reaches the review shim maps onto the CLI contract by
// shape: a call that cannot succeed as written exits 2, a runtime refusal
// exits 1.
func TestUsageLikeKindsAgreeOnExit(t *testing.T) {
	for _, k := range []Kind{KindUsage, KindRoundMismatch, KindUnknownThread,
		KindNoLiveRound, KindNotAnswered} {
		if k.ExitCode() != 2 {
			t.Errorf("%q exit code = %d, want 2 (change the call)", k, k.ExitCode())
		}
	}
	for _, k := range []Kind{KindAuthUnavailable, KindRateLimited, KindGitHubError, KindRefused} {
		if k.ExitCode() != 1 {
			t.Errorf("%q exit code = %d, want 1 (retry)", k, k.ExitCode())
		}
	}
	// The control-layer kinds stay 1 on purpose: their CLI exits come from
	// prose (ADR-0008 predates Reason), so 2 here would invent a usage exit
	// nobody implemented. Pin it so the choice cannot drift unnoticed.
	for _, k := range []Kind{KindBadRequest, KindBadJSON, KindUnknownCmd,
		KindUnknownJob, KindNotRunning, KindAlreadyRunning, KindAlreadyDone,
		KindUnsupported} {
		if k.ExitCode() != 1 {
			t.Errorf("%q exit code = %d, want 1 (control verbs exit off prose)", k, k.ExitCode())
		}
	}
	if KindUsage == KindBadRequest {
		t.Fatal("usage and bad_request are distinct published values; do not collapse them")
	}
	// An unknown or empty reason is a runtime refusal, never a guessed usage
	// error — the same rule the review CLI applies before it converts.
	if Kind("").ExitCode() != 1 || Kind("no-such-reason").ExitCode() != 1 {
		t.Fatal("unknown reasons must exit 1")
	}
}
