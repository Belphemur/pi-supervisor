// Package version holds the build-time version string for pi-supervisor.
//
// It is injected at link time by install.sh and the CI build:
//
//	go build -trimpath -ldflags="-s -w -ldflags=-X internal/version.ver=<git-rev>" \
//	    -o pi-supervisor ./cmd/pi-supervisor
//
// When the binary is built without the -X injection (e.g. `go build` in a
// developer tree, or the go:embed module cache), ver is the empty string and
// Version() falls back to "dev" so the binary is always identifiable.
package version

// ver is the linker-injected version string. The default value "dev" is set
// here as a package-level variable so that a build without -X still returns a
// sensible string rather than "".
var ver = "dev"

// Version returns the version string embedded at link time, or "dev" when the
// binary was built without injection (developer build, module cache, etc.).
func Version() string {
	return ver
}
