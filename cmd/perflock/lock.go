// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"fmt"
	"runtime"
	"sync"

	"github.com/aclements/perflock/internal/cpuset"
	"golang.org/x/sys/unix"
)

type PerfLock struct {
	l sync.Mutex
	q []*Locker

	cores unix.CPUSet
}

type Locker struct {
	C             <-chan bool
	c             chan<- bool
	wantCores     uint // Desired number of cores.
	availCores    unix.CPUSet
	assignedCores unix.CPUSet
	shared        bool
	woken         bool

	msg string
}

func (l *PerfLock) Enqueue(shared, nonblocking bool, wantCores uint, set unix.CPUSet, msg string) *Locker {
	ch := make(chan bool, 1)
	locker := &Locker{
		C:          ch,
		c:          ch,
		wantCores:  wantCores,
		availCores: set,
		shared:     shared,
		woken:      false,
		msg:        msg,
	}

	// Enqueue.
	l.l.Lock()
	defer l.l.Unlock()
	l.setQ(append(l.q, locker))

	if nonblocking && !locker.woken {
		// Acquire failed. Dequeue.
		l.setQ(l.q[:len(l.q)-1])
		return nil
	}

	return locker
}

func (l *PerfLock) Dequeue(locker *Locker) {
	l.l.Lock()
	defer l.l.Unlock()
	for i, o := range l.q {
		if locker == o {
			newset := cpuset.Union(theLock.cores, locker.assignedCores) // Can re-use these cores
			vlog("disconnected: adding cores back to the set: %s\n\tavail\t%s\n\tfreed\t%s\n\tresult\t%s",
				locker.msg, cpuset.String(theLock.cores), cpuset.String(locker.assignedCores), cpuset.String(newset))
			theLock.cores = newset
			copy(l.q[i:], l.q[i+1:])
			l.setQ(l.q[:len(l.q)-1])
			return
		}
	}
	panic("Dequeue of non-enqueued Locker")
}

func (l *PerfLock) Queue() []string {
	var q []string

	l.l.Lock()
	defer l.l.Unlock()
	for _, locker := range l.q {
		q = append(q, locker.msg)
	}
	return q
}

// l.l must be held.
func (l *PerfLock) setQ(q []*Locker) {
	l.q = q
	if len(q) == 0 {
		return
	}

	wake := func(locker *Locker) {
		if locker.woken == false {
			l.takeCores(locker)
			locker.woken = true
			locker.c <- true
		}
	}
	if q[0].shared {
		// Wake all shared acquires (pending core constraints) at the head of the
		// queue.
		for i, locker := range q {
			vlog("AKTAU: %d: %+v\n", i, locker)
			if !locker.shared {
				break // Exclusive lock, but q[0] is shared (and already activated).
			}
			if i != 0 && !locker.woken {
				// TODO(aktau): Technically it's possible that the intersection of
				if locker.wantCores != 0 && uint(l.cores.Count()) < locker.wantCores {
					break // Not enough cores available.
				}
			}
			wake(locker)
		}
	} else {
		wake(q[0])
	}
}

// Reserves cores for use, if desired. Relevant for scheduling. Does not
// physically assign them yet, as the client itself does that using
// sched_setaffinity(2). No effect if the locker does not want any cores
// (`locker.wantCores == 0`).
//
// LOCKS_HELD: l.l
func (l *PerfLock) takeCores(locker *Locker) {
	assert(uint(l.cores.Count()) >= locker.wantCores, "BUG: %d < %d", l.cores.Count(), locker.wantCores)

	// If locker.wantCores == 0, assign all cores that the pid itself has access
	// to. This will not stop other shared tasks with wantCores == 0 from running.
	// But it will stop (shared) tasks which reserve cores.
	//
	// There's a less-than-ideal situation that can happen with the current
	// approach. If the user submits a mix of jobs that require cores and those that don't
	// require cores, we currently do:
	//
	//   cores = 8
	//   ---------
	//   J1 -shared -cores 2
	//   J2 -shared -cores 2
	//   J3 -shared
	//   J4 -shared -cores 2
	//
	// Then J3 will run on all cores, potentially disturbing the rest. We could
	// prevent `-shared` without `-cores` jobs from running if there are
	// `shared+cores` jobs active, but that also feels wrong. A better solution
	// is to use exclusive CPU sets (via the cgroups v2 API). That way the
	// `shared+cores` jobs can be sure only they can use those cores, and we don't
	// have to care about scheduling leftover tasks. The disadvantage is that this
	// requires privileges to execute, and is more finnicky to implement.
	if locker.wantCores == 0 {
		return
	}

	// Filter the CPUs that the application can schedule on (`locker.availCores`)
	// down to those not already taken by other lockers.
	cores := cpuset.Intersect(l.cores, locker.availCores)

	// Select wantCores contiguous cores.
	//
	// TODO(aktau): Leave as much space as possible between CPU sets of running
	//              tasks, to minimize cache adjacency effects. Alternatively,
	//              try to combine cpuset.cpu_exlusive and cpuset.mem_exclusive.
	want := locker.wantCores
	cpuset.Range(cores, func(i int) {
		if want > 0 {
			locker.assignedCores.Set(i) // Assigned.
			l.cores.Clear(i)            // Taken.
			want--
		}
	})

	assert(uint(locker.assignedCores.Count()) == locker.wantCores, "BUG: %d != %d", locker.assignedCores.Count(), locker.wantCores)
}

func assert(cond bool, format string, a ...interface{}) {
	if !cond {
		meta := ""
		var pcs [1]uintptr
		if runtime.Callers(1, pcs[:]) == 1 {
			frame, _ := runtime.CallersFrames(pcs[:]).Next()
			meta = fmt.Sprintf("%s (%s:%d): ", frame.Function, frame.File, frame.Line)
		}
		panic(fmt.Errorf("assert: "+meta+format, a...))
	}
}
