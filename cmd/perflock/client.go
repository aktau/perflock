// Copyright 2017 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package main

import (
	"encoding/gob"
	"fmt"
	"log"
	"net"
	"os"
)

type Client struct {
	c net.Conn

	gr *gob.Encoder
	gw *gob.Decoder
}

func NewClient(socketPath string) *Client {
	c, err := net.Dial("unix", socketPath)
	if err != nil {
		log.Print(err)
		log.Fatal("Is the perflock daemon running?")
	}

	gr, gw := gob.NewEncoder(c), gob.NewDecoder(c)

	return &Client{c, gr, gw}
}

func (c *Client) do(action PerfLockAction, response interface{}) {
	vlog("-> (%T) %+v\n", action.Action, action.Action)
	err := c.gr.Encode(action)
	if err != nil {
		log.Fatal(err)
	}

	err = c.gw.Decode(response)
	vlog("<- (%T) %+v\n", response, response)
	if err != nil {
		log.Fatal(err)
	}
}

func (c *Client) Acquire(shared, nonblocking bool, cores uint, msg string) *ActionAcquireResponse {
	var resp ActionAcquireResponse
	c.do(PerfLockAction{ActionAcquire{Pid: os.Getpid(), Shared: shared, Cores: cores, NonBlocking: nonblocking, Msg: msg}}, &resp)
	return &resp
}

func (c *Client) List() []string {
	var list []string
	c.do(PerfLockAction{ActionList{}}, &list)
	return list
}

func (c *Client) SetGovernor(percent int) error {
	var err string
	c.do(PerfLockAction{ActionSetGovernor{Percent: percent}}, &err)
	if err == "" {
		return nil
	}
	return fmt.Errorf("%s", err)
}
