// Copyright 2011 Google Inc.
// Author: Brad Fitzpatrick <bradfitz@golang.org>
// See LICENSE file in root.

// Package avr implements support for controlling Denon AVR receivers.
// In particular, this package was designed to control an AVR-3312CI.
//
// Be sure to put the receiver into network stay-awake mode if you want
// to be able to wake it from sleep. This draws a bit more power in
// standby.
package avr

import (
	"bufio"
	"errors"
	"log"
	"net"
	"strings"
	"sync"
)

// New returns a new Amp. The amp is safe for use by use by
// concurrent multiple goroutines. Broken TCP connections are
// retried as needed. When finished, call Close.
func New(addr string) *Amp {
	a := &Amp{
		addr:     addr,
		reqc:     make(chan request),
		ampc:     make(chan *ampLine),
		connerrc: make(chan error),
	}
	a.startConnect()
	go a.loop()
	return a
}

// Amp represents an AVR Receiver.
type Amp struct {
	// Immutable:
	addr     string
	reqc     chan request
	ampc     chan *ampLine
	connerrc chan error

	// Guarded by mu:
	mu             sync.Mutex
	closed         bool
	state          state
	stateListeners []chan error // nil for connected
	conn           *conn
	err            error
}

// Addr returns the address of the amp.
func (a *Amp) Addr() string {
	return a.addr
}

func (a *Amp) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed {
		return nil
	}
	a.closed = true
	close(a.reqc)
	if a.conn != nil {
		a.conn.c.Close()
	}
	return nil
}

func (a *Amp) Ping() error {
	a.startConnect() // no-op if already connected/connecting
	ch := make(chan *response)
	a.reqc <- request{ch: ch, cmd: pingCmd}
	res := <-ch
	return res.err
}

func (a *Amp) SendCommand(cmd string) error {
	a.startConnect() // no-op if already connected/connecting
	ch := make(chan *response)
	a.reqc <- request{ch: ch, cmd: rawCmd, raw: cmd}
	res := <-ch
	return res.err
}

func (a *Amp) startConnect() {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.closed || a.state != unconnected {
		return
	}
	a.state = connecting
	go a.connect()
}

// must be called with mu held
func (a *Amp) setState(err error) {
	if err == nil {
		a.state = connected
	} else {
		a.state = unconnected
	}
	a.err = err
	for _, ch := range a.stateListeners {
		ch <- err
	}
	a.stateListeners = nil
}

func (a *Amp) addStateListener(ch chan error) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.state == connecting {
		a.stateListeners = append(a.stateListeners, ch)
	} else {
		ch <- a.err
	}
}

func (a *Amp) connect() {
	c, err := net.Dial("tcp", a.addr)
	log.Printf("net.Dial: c=%v, err=%v", c, err)
	a.mu.Lock()
	defer a.mu.Unlock()
	a.setState(err)
	if err != nil {
		return
	}

	a.conn = &conn{
		a:    a,
		c:    c,
		bufr: bufio.NewReader(c),
		bufw: bufio.NewWriter(c),
	}
	go a.conn.readFromAmp()
}

func (a *Amp) loop() {
	for {
		select {
		case req, ok := <-a.reqc:
			if !ok {
				return
			}
			a.handleRequest(req)
		case ampl := <-a.ampc:
			log.Printf("amp says: %q", ampl.l)
		case <-a.connerrc:
			a.startConnect()
		}
	}
}

// run in loop goroutine
func (a *Amp) handleRequest(req request) {
	switch req.cmd {
	case pingCmd:
		a.handlePing(req)
	case rawCmd:
		a.handleRaw(req)
	default:
		log.Printf("unhandled command request: %#v", req)
	}
}

// run in loop goroutine
func (a *Amp) handlePing(req request) {
	a.mu.Lock()
	st := a.state
	a.mu.Unlock()

	if st == connected {
		req.ch <- &response{err: nil}
	}

	a.startConnect()
	ch := make(chan error)
	a.addStateListener(ch)

	req.ch <- &response{err: <-ch}
}

// run in loop goroutine
func (a *Amp) handleRaw(req request) {
	a.mu.Lock()
	st := a.state
	conn := a.conn
	a.mu.Unlock()

	if st != connected {
		req.ch <- &response{err: errors.New("not connected")}
		return
	}

	raw := req.raw
	if !strings.HasSuffix(raw, "\r") {
		raw += "\r"
	}
	conn.bufw.WriteString(raw)
	conn.bufw.Flush()
	req.ch <- &response{err: nil}
}

// conn is a single TCP connection to an AVR. If it fails, the
// amp makes a new one.
type conn struct {
	// All immutable:
	a    *Amp
	c    net.Conn
	bufr *bufio.Reader
	bufw *bufio.Writer
}

type state int

const (
	unconnected state = iota
	connecting
	connected
)

type command int

const (
	pingCmd command = iota
	rawCmd
)

type request struct {
	ch  chan *response
	cmd command

	// If rawCmd
	raw string
}

type response struct {
	err error // for ping
}

func (c *conn) readFromAmp() {
	for {
		bs, err := c.bufr.ReadSlice('\r')
		if err != nil {
			c.a.connerrc <- err
			return
		}
		c.a.ampc <- newAmpLine(string(bs))
	}
}

type ampLine struct {
	l string
}

func newAmpLine(s string) *ampLine {
	return &ampLine{l: s}
}
