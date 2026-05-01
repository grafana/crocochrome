// export_test.go exposes unexported symbols for black-box tests in package crocochrome_test.
// This file is only compiled during `go test`; it does not affect the production binary.
package crocochrome

// ReadOOMKillCount is the test-visible alias for readOOMKillCount.
var ReadOOMKillCount func(path string) (uint64, error) = readOOMKillCount

// CgroupV1MemoryOOMControlPath is the test-visible alias for cgroupV1MemoryOOMControlPath.
var CgroupV1MemoryOOMControlPath func(procRoot string) string = cgroupV1MemoryOOMControlPath
