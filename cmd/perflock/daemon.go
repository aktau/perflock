// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/gob"
	"fmt"
	"io"
	"log"
	"net"
	"os"
	"os/user"
	"runtime"
	"time"

	"github.com/aclements/perflock/internal/cpupower"
	"github.com/aclements/perflock/internal/cpuset"
	"golang.org/x/sys/unix"
	"inet.af/peercred"
)

var theLock PerfLock

var allCores unix.CPUSet

func doDaemon(path string) {
	// TODO(aktau): Don't just assume that pid 0's cpuset is the full system
	// cpuset. Perhaps it's allowed mask would be that, though...
	var err error
	allCores, err = cpuset.CPUSetOfPid(1)
	if err != nil {
		panic(err)
	}
	// TODO(aktau): How to deal with changing (system-level) CPU masks?
	//
	// An admin could (e.g.) disable hyperthreading at runtime this way:
	//
	//  $ echo off | sudo tee /sys/devices/system/cpu/smt/control
	//
	// For now we punt on this issue, and hope they restart the daemon after doing
	// this. We could poll whether allCores still matches the definition we have,
	// and if so (a) no longer accept new tasks and (b) exit as soon as all
	// current tasks are done. If we're running via a process manager (like
	// systemd), it will restart us.
	theLock.cores = allCores

	// TODO: Don't start if another daemon is already running.

	// Linux supports an abstract namespace for UNIX domain sockets (see unix(7)).
	// These do not involve the filesystem, and are world-connectable.
	isAbstractSocket := runtime.GOOS == "linux" && len(path) > 1 && path[0] == '@'
	if !isAbstractSocket {
		os.Remove(path)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		log.Fatal(err)
	}
	defer l.Close()
	if !isAbstractSocket {
		err = os.Chmod(path, 0777)
		if err != nil {
			log.Fatal(err)
		}
	}

	// Receive connections.
	for {
		conn, err := l.Accept()
		if err != nil {
			log.Fatal(err)
		}

		go func(c net.Conn) {
			defer c.Close()
			NewServer(c).Serve()
		}(conn)
	}
}

type Server struct {
	c        net.Conn
	userName string

	locker    *Locker
	acquiring bool

	oldGovernors []*governorSettings
}

func send(enc *gob.Encoder, a interface{}) bool {
	if err := enc.Encode(a); err != nil {
		log.Printf("could not send response %T %v to user: %v", a, a, err)
		return false
	}
	vlog("-> %T %+v\n", a, a)
	return true
}

func NewServer(c net.Conn) *Server {
	return &Server{c: c}
}

func (s *Server) Serve() {
	// Drop any held locks if we exit for any reason.
	defer s.drop()

	// Get connection credentials.
	cred, err := peercred.Get(s.c)
	if err != nil {
		log.Print("reading credentials: ", err)
		return
	}

	s.userName = "???"
	if uid, ok := cred.UserID(); ok {
		if u, err := user.LookupId(uid); err == nil {
			s.userName = u.Username
		}
	}

	// Receive incoming actions. We do this in a goroutine so the
	// main handler can select on EOF or lock acquisition.
	actions := make(chan PerfLockAction)
	go func() {
		gr := gob.NewDecoder(s.c)
		for {
			var msg PerfLockAction
			err := gr.Decode(&msg)
			if err != nil {
				if err != io.EOF {
					log.Print(err)
				}
				close(actions)
				return
			}
			vlog("<- %T%+v\n", msg.Action, msg.Action)
			actions <- msg
		}
	}()

	// Process incoming actions.
	var acquireC <-chan bool
	gw := gob.NewEncoder(s.c)
	for {
		select {
		case action, ok := <-actions:
			if !ok {
				// Connection closed.
				return
			}
			if s.acquiring {
				log.Printf("protocol error: message while acquiring")
				return
			}
			switch action := action.Action.(type) {
			case ActionAcquire:
				if s.locker != nil {
					log.Printf("protocol error: acquiring lock twice")
					return
				}
				msg := fmt.Sprintf("%s\t%s\t%s\tcores=%d", s.userName, time.Now().Format(time.Stamp), action.Msg, action.Cores)
				if action.Shared {
					msg += " [shared]"
				}
				availCores, err := cpuset.CPUSetOfPid(action.Pid)
				if err != nil {
					log.Printf("cannot determine CPU set of pid %d: %v", action.Pid, err)
					return
				}
				if action.Cores > uint(availCores.Count()) {
					send(gw, ActionAcquireResponse{
						Err: fmt.Errorf("requested %d cores, but process only has %d available (system has %d)", action.Cores, availCores.Count(), allCores.Count()).Error(),
					})
					return
				}
				s.locker = theLock.Enqueue(action.Shared, action.NonBlocking, action.Cores, availCores, msg)
				if s.locker != nil {
					// Enqueued. Wait for acquire.
					s.acquiring = true
					acquireC = s.locker.C
				} else {
					// Non-blocking acquire failed.
					if !send(gw, ActionAcquireResponse{}) {
						return
					}
				}

			case ActionList:
				list := theLock.Queue()
				if !send(gw, list) {
					return
				}

			case ActionSetGovernor:
				if s.locker == nil {
					log.Printf("protocol error: setting governor without lock")
					return
				}
				err := s.setGovernor(action.Percent)
				errString := ""
				if err != nil {
					errString = err.Error()
				}
				if !send(gw, errString) {
					return
				}

			default:
				log.Printf("unknown message")
				return
			}

		case <-acquireC:
			// Lock acquired.
			s.acquiring, acquireC = false, nil
			if !send(gw, ActionAcquireResponse{Acquired: true, Cores: s.locker.assignedCores}) {
				return
			}
		}
	}
}

func (s *Server) drop() {
	// Restore the CPU governor before releasing the lock.
	if s.oldGovernors != nil {
		s.restoreGovernor()
		s.oldGovernors = nil
	}
	// Release the lock.
	if s.locker != nil {
		theLock.Dequeue(s.locker)
		s.locker = nil
	}
}

type governorSettings struct {
	domain   *cpupower.Domain
	min, max int
}

func (s *Server) setGovernor(percent int) error {
	domains, err := cpupower.Domains()
	if err != nil {
		return err
	}
	if len(domains) == 0 {
		return fmt.Errorf("no power domains")
	}

	// Save current frequency settings.
	old := []*governorSettings{}
	for _, d := range domains {
		min, max, err := d.CurrentRange()
		if err != nil {
			return err
		}
		old = append(old, &governorSettings{d, min, max})
	}
	s.oldGovernors = old

	// Set new settings.
	abs := func(x int) int {
		if x < 0 {
			return -x
		}
		return x
	}
	for _, d := range domains {
		min, max, avail := d.AvailableRange()
		target := (max-min)*percent/100 + min

		// Find the nearest available frequency.
		if len(avail) != 0 {
			closest := avail[0]
			for _, a := range avail {
				if abs(target-a) < abs(target-closest) {
					closest = a
				}
			}
			target = closest
		}

		err := d.SetRange(target, target)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Server) restoreGovernor() error {
	var err error
	for _, g := range s.oldGovernors {
		// Try to set all of the domains, even if one fails.
		err1 := g.domain.SetRange(g.min, g.max)
		if err1 != nil && err == nil {
			err = err1
		}
	}
	return err
}
