package version

import (
	"runtime/debug"
	"slices"
	"strings"
	"sync"
)

const (
	revisionKey = "vcs.revision"
	timeKey     = "vcs.time"
)

// version is set at build time with -ldflags "-X github.com/grafana/crocochrome/internal/version.version=...", see the
// Makefile. It holds the output of scripts/version, which is also used as the image tag, so the version reported by
// the binary and the image tag are always the same string.
//
// Go's own main module version cannot be used for this: on an untagged commit it is a pseudo-version
// (v0.13.4-0.20261028143447-5541944d06b2), while scripts/version gives `git describe` output (v0.13.3-1-g5541944).
// On a tagged commit both are the tag.
//
//nolint:gochecknoglobals // This variable is only written by the linker.
var version string

// Short returns the version injected at build time, or the version of the main module if none was injected (for
// example when building with a plain `go build` or `go install`).
func Short() string {
	if version != "" {
		return version
	}

	bi := getBuildInfo()

	return bi.Main.Version
}

func Commit() string {
	return getBuildInfoByKey(revisionKey)
}

func Buildstamp() string {
	return getBuildInfoByKey(timeKey)
}

func getBuildInfoByKey(key string) string {
	bi := getBuildInfo()
	if bi == nil {
		return "invalid"
	}

	idx, found := slices.BinarySearchFunc(bi.Settings, key, isKey)
	if found {
		return bi.Settings[idx].Value
	}

	return "unknown"
}

//nolint:gochecknoglobals // This variable is only accessed in this package.
var getBuildInfo = sync.OnceValue(func() *debug.BuildInfo {
	bi, ok := debug.ReadBuildInfo()
	if !ok {
		return nil
	}

	slices.SortFunc(bi.Settings, cmpBuildSettings)

	return bi
})

func cmpBuildSettings(a, b debug.BuildSetting) int {
	if v := strings.Compare(a.Key, b.Key); v != 0 {
		return v
	}

	return strings.Compare(a.Value, b.Value)
}

func isKey(a debug.BuildSetting, key string) int {
	return strings.Compare(a.Key, key)
}
