package version

import (
	"runtime/debug"
	"testing"
)

// These tests modify the package-level version variable, so they must not run in parallel.

func TestShortUsesVersionInjectedAtBuildTime(t *testing.T) {
	// On an untagged commit, Go reports a pseudo-version for the main module, while the image tag comes from
	// scripts/version (git describe). The injected value is what keeps both in sync.
	const injected = "v0.13.3-1-g5541944"

	t.Cleanup(func(old string) func() { return func() { version = old } }(version))
	version = injected

	if got := Short(); got != injected {
		t.Errorf("Short() = %q, want %q", got, injected)
	}
}

func TestShortFallsBackToModuleVersion(t *testing.T) {
	t.Cleanup(func(old string) func() { return func() { version = old } }(version))
	version = ""

	bi, ok := debug.ReadBuildInfo()
	if !ok {
		t.Fatal("build info is not available")
	}

	if got := Short(); got != bi.Main.Version {
		t.Errorf("Short() = %q, want the main module version %q", got, bi.Main.Version)
	}
}
