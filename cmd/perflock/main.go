// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Command perflock is a simple locking wrapper for running benchmarks
// on shared hosts.
//
// The typical use of perflock is:
//
//	perflock [-shared] command...
//
// This will acquire a system-wide lock while running command.
//
// In exclusive mode (the default), perflock prevents any other
// perflock'd command from running. This should be used for running
// benchmarks that are sensitive to the environment. In the future,
// this may do other things to set up a better isolated benchmarking
// environment.
//
// In shared mode (with the -shared flag), perflock can run other
// shared-mode commands concurrently. This should be used for commands
// that would perturb benchmarks but aren't themselves benchmarks.
//
// For convenience, we recommend you create shell aliases for
// perflock:
//
//	alias pl=perflock
//	alias pls='perflock -shared'
//
// perflock depends on a locking daemon, which can be started with
// perflock -daemon.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/aclements/perflock/internal/cpuset"
	"golang.org/x/sys/unix"
)

var (
	gVerbose  = false
	gIsClient = true
)

func main() {
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "Usage of %s:\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "  %s [flags] command...\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -list\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "  %s -daemon\n", os.Args[0])
		fmt.Fprintf(os.Stderr, "\n")
		flag.PrintDefaults()
	}
	flagDaemon := flag.Bool("daemon", false, "start perflock daemon")
	flagList := flag.Bool("list", false, "print current and pending commands")
	flagSocket := flag.String("socket", "/var/run/perflock.socket", "connect to socket `path`")
	flagShared := flag.Bool("shared", false, "acquire lock in shared mode (default: exclusive mode)")
	flagVerbose := flag.Bool("verbose", false, "be verbose, useful for debuggging")
	flagCores := flag.Uint("cores", 0, "how many cores to reserve")
	flagGovernor := &governorFlag{percent: 90}
	flag.Var(flagGovernor, "governor", "set CPU frequency to `percent` between the min and max\n\twhile running command, or \"none\" for no adjustment")
	flag.Parse()
	gVerbose = *flagVerbose

	if *flagDaemon {
		gIsClient = false
		if flag.NArg() > 0 {
			flag.Usage()
			os.Exit(2)
		}
		doDaemon(*flagSocket)
		return
	}

	log.SetFlags(0)

	if *flagList {
		if flag.NArg() > 0 {
			flag.Usage()
			os.Exit(2)
		}
		c := NewClient(*flagSocket)
		list := c.List()
		for _, l := range list {
			fmt.Println(l)
		}
		return
	}

	cmd := flag.Args()
	if len(cmd) == 0 {
		flag.Usage()
		os.Exit(2)
	}
	c := NewClient(*flagSocket)
	resp := c.Acquire(*flagShared, true, *flagCores, shellEscapeList(cmd))
	if resp.Err != "" {
		fmt.Fprintf(os.Stderr, "invalid request: %v\n", resp.Err)
		return
	}
	if !resp.Acquired {
		list := c.List()
		fmt.Fprintf(os.Stderr, "Waiting for lock...\n")
		for _, l := range list {
			fmt.Fprintln(os.Stderr, l)
		}
		resp = c.Acquire(*flagShared, false, *flagCores, shellEscapeList(cmd))
	}
	if !*flagShared {
		if flagGovernor.percent >= 0 {
			c.SetGovernor(flagGovernor.percent)
		}
	}
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	if *flagCores > 0 {
		// TODO(aktau): Verify that we actually have the cores we want. It's
		//              possible to get a smaller set than the one desired:
		//              https://unix.stackexchange.com/a/732654.
		fmt.Printf("setting affinity to %s", cpuset.String(resp.Cores))
		err := unix.SchedSetaffinity(0, &resp.Cores)
		if err != nil {
			fmt.Printf("SchedSetaffinity: %v", err)
		}
	}
	ignoreSignals()
	run(cmd)
}

// vlog logs if gVerbose is true.
func vlog(format string, a ...interface{}) {
	if gVerbose {
		logfn := log.Printf
		if gIsClient {
			logfn = func(format string, a ...interface{}) {
				fmt.Fprintf(os.Stderr, format, a...)
			}
		}
		logfn(format, a...)
	}
}

type governorFlag struct {
	percent int
}

func (f *governorFlag) String() string {
	if f.percent < 0 {
		return "none"
	}
	return fmt.Sprintf("%d%%", f.percent)
}

func (f *governorFlag) Set(v string) error {
	if v == "none" {
		f.percent = -1
	} else {
		m := regexp.MustCompile(`^([0-9]+)%$`).FindStringSubmatch(v)
		if m == nil {
			return fmt.Errorf("governor must be \"none\" or \"N%%\"")
		}
		f.percent, _ = strconv.Atoi(m[1])
	}
	return nil
}

// run executes args as a command and exits with the command's exit
// status.
func run(args []string) {
	cmd := exec.Command(args[0], args[1:]...)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	err := cmd.Run()
	switch err := err.(type) {
	case nil:
		os.Exit(0)
	case *exec.ExitError:
		status := err.Sys().(syscall.WaitStatus)
		if status.Exited() {
			os.Exit(status.ExitStatus())
		}
		log.Fatal(err)
	default:
		log.Fatal(err)
	}
}

// shellEscape escapes a single shell token.
func shellEscape(x string) string {
	if len(x) == 0 {
		return "''"
	}
	for _, r := range x {
		if 'a' <= r && r <= 'z' || 'A' <= r && r <= 'Z' || '0' <= r && r <= '9' || strings.ContainsRune("@%_-+:,./", r) {
			continue
		}
		// Unsafe character.
		return "'" + strings.Replace(x, "'", "'\"'\"'", -1) + "'"
	}
	return x
}

// shellEscapeList escapes a list of shell tokens.
func shellEscapeList(xs []string) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = shellEscape(x)
	}
	return strings.Join(out, " ")
}
