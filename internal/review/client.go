package review

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/bradleyfalzon/ghinstallation/v2"
	"github.com/google/go-github/v90/github"
	"github.com/shurcooL/githubv4"
)

// Client is the daemon's GitHub handle. API assignment is deliberate and not
// interchangeable (ADR-0012 §3.2):
//
//   - GraphQL for review threads. It is the ONLY surface carrying thread
//     identity, isResolved and comment authors; the REST pulls-comments
//     endpoint carries none of the three.
//   - REST for PR/CI metadata (head sha, PR state, Actions job conclusions).
type Client struct {
	rest *github.Client
	gql  *githubv4.Client
	http *http.Client
}

// NewClient builds a Client from the environment: a GitHub App when
// GITHUB_APP_ID + GITHUB_APP_PRIVATE_KEY are set, else the `gh auth token`
// fallback. `gh` is invoked for token acquisition only — never for an API call.
//
// Both clients share ONE http.Client, so there is a single token source and a
// single auth path to keep in sync (ADR-0012 §6).
func NewClient(ctx context.Context, owner, repo string) (*Client, error) {
	tok, err := tokenSource(ctx, owner, repo)
	if err != nil {
		return nil, err
	}
	hc := &http.Client{Transport: tok, Timeout: 30 * time.Second}
	rest, err := github.NewClient(github.WithHTTPClient(hc))
	if err != nil {
		return nil, refuse(ReasonGitHubError, "build rest client: %v", err)
	}
	return &Client{
		rest: rest,
		gql:  githubv4.NewClient(hc),
		http: hc,
	}, nil
}

// tokenSource prefers a GitHub App installation token, falling back to the
// active `gh` token. Priority is fixed so a deployment never silently changes
// identity between runs.
//
// ghinstallation owns JWT minting AND installation-token refresh (60-minute
// TTL, auto-renewed), so the daemon holds no hand-rolled refresh loop.
func tokenSource(ctx context.Context, owner, repo string) (http.RoundTripper, error) {
	appID := os.Getenv("GITHUB_APP_ID")
	keyRef := os.Getenv("GITHUB_APP_PRIVATE_KEY")
	if appID != "" && keyRef != "" {
		return appTransport(ctx, appID, keyRef, owner, repo)
	}
	tok, err := ghCLIToken(ctx)
	if err != nil {
		return nil, err
	}
	return &staticAuth{tok: tok}, nil
}

// appTransport builds the App transport from an inline PEM or a key path, so
// a systemd unit can pass either without a helper file on disk.
//
// ghinstallation's two-stage shape: an AppsTransport mints the App JWT, then
// the installation-scoped Transport caches and auto-renews the 1h token.
func appTransport(ctx context.Context, appID, keyRef, owner, repo string) (http.RoundTripper, error) {
	var id int64
	if _, err := fmt.Sscanf(appID, "%d", &id); err != nil || id <= 0 {
		return nil, refuse(ReasonAuthUnavailable, "GITHUB_APP_ID %q is not a positive integer", appID)
	}
	key := []byte(keyRef)
	if !strings.Contains(keyRef, "-----BEGIN") {
		b, err := os.ReadFile(keyRef)
		if err != nil {
			return nil, refuse(ReasonAuthUnavailable, "GITHUB_APP_PRIVATE_KEY: %v", err)
		}
		key = b
	}
	atr, err := ghinstallation.NewAppsTransport(http.DefaultTransport, id, key)
	if err != nil {
		return nil, refuse(ReasonAuthUnavailable, "GitHub App key rejected: %v", err)
	}
	// Resolve the installation covering the reviewed repo once, at
	// construction: a campaign targets a fixed repo, so re-resolving per poll
	// would be a second source of truth for the same fact.
	instID, err := findInstallation(ctx, atr, owner, repo)
	if err != nil {
		return nil, err
	}
	tr := ghinstallation.NewFromAppsTransport(atr, instID)
	return tr, nil
}

// findInstallation asks GitHub which installation of the App covers a repo.
// The lookup itself is authenticated with the App JWT.
func findInstallation(ctx context.Context, atr *ghinstallation.AppsTransport, owner, repo string) (int64, error) {
	c, err := github.NewClient(github.WithHTTPClient(&http.Client{
		Transport: atr,
		Timeout:   30 * time.Second,
	}))
	if err != nil {
		return 0, refuse(ReasonAuthUnavailable, "build app client: %v", err)
	}
	inst, _, err := c.Apps.GetRepositoryInstallation(ctx, owner, repo)
	if err != nil {
		return 0, refuse(ReasonAuthUnavailable,
			"no GitHub App installation for %s/%s: %v", owner, repo, err)
	}
	return inst.GetID(), nil
}

// ghCLIToken runs `gh auth token` — the only gh invocation in the daemon's
// lifecycle — and returns it as a static bearer token.
func ghCLIToken(ctx context.Context) (string, error) {
	out, err := exec.CommandContext(ctx, "gh", "auth", "token").Output()
	if err != nil {
		return "", refuse(ReasonAuthUnavailable,
			"GitHub auth unavailable: run 'gh auth login' or set GITHUB_APP_ID")
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", refuse(ReasonAuthUnavailable, "gh auth token returned empty")
	}
	return tok, nil
}

// staticAuth sets a bearer token on every request.
type staticAuth struct{ tok string }

func (s *staticAuth) RoundTrip(req *http.Request) (*http.Response, error) {
	r := req.Clone(req.Context())
	r.Header.Set("Authorization", "Bearer "+s.tok)
	return http.DefaultTransport.RoundTrip(r)
}
