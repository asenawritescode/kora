//go:build !linux

package engine

// RSS sampling is currently implemented for Linux, the supported self-hosted
// deployment target. Unsupported platforms report zero rather than a guessed
// value; heap allocation and system gauges remain available everywhere.
func processRSSBytes() int64 { return 0 }
