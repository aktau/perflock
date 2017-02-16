// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import "encoding/gob"

type PerfLockAction struct {
	Action interface{}
}

// ActionAcquire acquires the lock. The response is a boolean
// indicating whether or not the lock was acquired (which may be false
// for a non-blocking acquire).
type ActionAcquire struct {
	Shared      bool
	NonBlocking bool
	Msg         string
}

// ActionList returns the list of current and pending lock
// acquisitions as a []string.
type ActionList struct {
}

func init() {
	gob.Register(ActionAcquire{})
	gob.Register(ActionList{})
}
