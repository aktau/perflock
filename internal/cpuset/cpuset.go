// Package cpuset contains helpers for the CPUSet functionality in
// golang.org/x/sys/unix.
package cpuset

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Parse CPUSet constructs a new CPU set from a Linux CPU list formatted string.
//
// See: http://man7.org/linux/man-pages/man7/cpuset.7.html#FORMATS
//
// Code adapted from https://github.com/kubernetes/kubernetes/blob/v1.27.10/pkg/kubelet/cm/cpuset/cpuset.go#L201
//
// Apache License 2.0
func Parse(s string) (unix.CPUSet, error) {
	var set unix.CPUSet

	// Handle empty string.
	if s == "" {
		return set, errors.New("cannot parse empty string")
	}

	// Split CPU list string:
	// "0-5,34,46-48" => ["0-5", "34", "46-48"]
	ranges := strings.Split(s, ",")

	for _, r := range ranges {
		boundaries := strings.SplitN(r, "-", 2)
		if len(boundaries) == 1 {
			// Handle ranges that consist of only one element like "34".
			elem, err := strconv.Atoi(boundaries[0])
			if err != nil {
				return set, err
			}
			set.Set(elem)
		} else if len(boundaries) == 2 {
			// Handle multi-element ranges like "0-5".
			start, err := strconv.Atoi(boundaries[0])
			if err != nil {
				return set, err
			}
			end, err := strconv.Atoi(boundaries[1])
			if err != nil {
				return set, err
			}
			if start > end {
				return set, fmt.Errorf("invalid range %q (%d > %d)", r, start, end)
			}
			// start == end is acceptable (1-1 -> 1)

			// Add all elements to the result.
			// e.g. "0-5", "46-48" => [0, 1, 2, 3, 4, 5, 46, 47, 48].
			for e := start; e <= end; e++ {
				set.Set(e)
			}
		}
	}
	return set, nil
}

func allowedList(pid int) (string, error) {
	filename := fmt.Sprintf("/proc/%d/status", pid)
	b, err := os.ReadFile(filename)
	if err != nil {
		return "", err
	}

	const item = "Cpus_allowed_list:"
	_, b, found := bytes.Cut(b, []byte(item))
	if !found {
		return "", fmt.Errorf("did not find %q in %q", item, filename)
	}

	b, _, found = bytes.Cut(b, []byte("\n"))
	if !found {
		return "", fmt.Errorf("expected to find a new line after %q", item)
	}

	b = bytes.TrimSpace(b)
	return string(b), nil
}

func CPUSetOfPid(pid int) (set unix.CPUSet, err error) {
	list, err := allowedList(pid)
	if err != nil {
		return set, err
	}
	return Parse(list)
}

func Intersect(a, b unix.CPUSet) unix.CPUSet {
	var res unix.CPUSet
	for i := range a {
		res[i] = a[i] & b[i]
	}
	return res
}

func Union(a, b unix.CPUSet) unix.CPUSet {
	var res unix.CPUSet
	for i := range a {
		res[i] = a[i] | b[i]
	}
	return res
}

func Difference(a, b unix.CPUSet) unix.CPUSet {
	var res unix.CPUSet
	for i := range a {
		res[i] = a[i] &^ b[i]
	}
	return res
}

// Range calls fn with the index of every CPU available in the set.
func Range(s unix.CPUSet, fn func(int)) {
	count := s.Count()
	for i := 0; count > 0; i++ {
		if s.IsSet(i) {
			fn(i)
			count--
		}
	}
}

var numCPUs = runtime.NumCPU()

const bytesPerChunk = unsafe.Sizeof(unix.CPUSet{}[0])

func String(s unix.CPUSet) string {
	var sb strings.Builder
	for i := 0; i < len(s) && i*8*int(bytesPerChunk) < numCPUs; i++ {
		fmt.Fprintf(&sb, "%08X ", s[i])
	}
	fmt.Fprintf(&sb, "total: %d", s.Count())
	return sb.String()
}
