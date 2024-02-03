// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/gob"

	"golang.org/x/sys/unix"
)

type PerfLockAction struct {
	Action interface{}
}

// ActionAcquire acquires the lock. The response is a boolean
// indicating whether or not the lock was acquired (which may be false
// for a non-blocking acquire).
type ActionAcquire struct {
	Pid   int  // The pid of the requester, used to see what sorts of permissions it has (CPU set...).
	Cores uint // 0 for "no limit"

	Shared      bool
	NonBlocking bool
	Msg         string
}

type ActionAcquireResponse struct {
	Acquired bool // If true, the other fields can be read.

	Cores unix.CPUSet // The cores on which to limit oneself.

	Err string
}

// ActionList returns the list of current and pending lock
// acquisitions as a []string.
type ActionList struct {
}

// ActionSetGovernor sets the CPU frequency of all CPUs. The caller
// must hold the lock.
type ActionSetGovernor struct {
	// Percent indicates the percent to set the CPU governor to
	// between the lower and highest available frequencies.
	Percent int
}

func init() {
	gob.Register(ActionAcquire{})
	gob.Register(ActionList{})
	gob.Register(ActionSetGovernor{})
}
