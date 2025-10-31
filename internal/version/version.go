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

func Short() string {
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
