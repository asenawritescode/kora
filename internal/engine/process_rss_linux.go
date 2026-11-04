//go:build linux

package engine

import (
	"os"
	"strconv"
	"strings"
)

func processRSSBytes() int64 {
	data, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0
	}
	fields := strings.Fields(string(data))
	if len(fields) < 2 {
		return 0
	}
	pages, err := strconv.ParseInt(fields[1], 10, 64)
	if err != nil || pages < 0 {
		return 0
	}
	return pages * int64(os.Getpagesize())
}
