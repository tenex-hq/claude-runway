package main

import (
	"runtime/debug"
	"strings"
)

// Version reporting has to work for three different build paths, which is more than one
// mechanism can cover:
//
//	1. A release build, where GoReleaser injects -X main.binVersion=<tag>.
//	2. `go install github.com/tenex-hq/claude-runway@v0.3.0`, which passes no ldflags but
//	   records the module version in the build info.
//	3. `go build` or `go install .` in a checkout, which records the VCS revision and
//	   whether the tree was dirty.
//
// Relying only on the ldflags meant paths 2 and 3 reported a hardcoded literal, which went
// stale the moment the next version was tagged and quietly lied about what was running.

// devVersion is the value binVersion holds when nothing stamped it. Compared by identity to
// detect an un-stamped build, so it must not look like a real version.
const devVersion = "dev"

// resolveVersion returns the most trustworthy version available, preferring an explicit stamp,
// then the module version, then the VCS revision. A dirty tree is always disclosed: a binary
// built from uncommitted changes is not the version it claims to be.
func resolveVersion() string {
	if binVersion != devVersion && binVersion != "" {
		return binVersion // stamped by a release build; the most specific answer there is
	}

	info, ok := debug.ReadBuildInfo()
	if !ok {
		return devVersion
	}

	var rev string
	dirty := false
	for _, s := range info.Settings {
		switch s.Key {
		case "vcs.revision":
			rev = s.Value
			if len(rev) > 7 {
				rev = rev[:7]
			}
		case "vcs.modified":
			dirty = s.Value == "true"
		}
	}

	// Go reports "(devel)" for a build with no resolvable module version, and an empty string
	// when there is no module context at all.
	base := strings.TrimPrefix(info.Main.Version, "v")
	if base == "" || base == "(devel)" {
		base = devVersion
	}
	// Go already appends its own "+dirty" to a version derived from a modified tree. Take that
	// as a second source of truth for dirtiness, then strip it, so the marker is not emitted
	// twice ("0.3.0+dirty+abc1234.dirty" was a real bug).
	if strings.HasSuffix(base, "+dirty") {
		dirty = true
		base = strings.TrimSuffix(base, "+dirty")
	}

	switch {
	case dirty && rev != "":
		// A binary built from uncommitted changes is not the version it names, so say both
		// which commit it started from and that it diverged.
		return base + "+" + rev + ".dirty"
	case dirty:
		return base + "+dirty"
	case base == devVersion && rev != "":
		// No module version to report, so the revision is all the identity there is.
		return base + "+" + rev
	default:
		return base
	}
}
