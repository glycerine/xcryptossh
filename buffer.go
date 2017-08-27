// Copyright 2012 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package ssh

import (
	"fmt"
	"io"
	"runtime"
	"strings"
	"sync"
	"time"
)

// errWhere satisfies net.Error
type errWhere struct {
	msg   string
	who   *idleTimer
	when  time.Time
	where []byte
}

func newErrTimeout(msg string, who *idleTimer) *errWhere {
	return newErrWhere("timeout:"+msg, who)
}

func stacktrace() []byte {
	sz := 512
	var stack []byte
	for {
		stack = make([]byte, sz)
		nw := runtime.Stack(stack, false)
		if nw >= sz {
			sz = sz * 2
		} else {
			stack = stack[:nw]
			break
		}
	}
	return stack
}

func newErrWhere(msg string, who *idleTimer) *errWhere {
	return &errWhere{msg: msg, who: who, when: time.Now(), where: stacktrace()}
}

func (e errWhere) Error() string {
	return fmt.Sprintf("%s, from idleTimer %p, generated at '%v'. stack='\n%v\n'",
		e.msg, e.who, e.when, string(e.where))
}

func (e errWhere) Timeout() bool {
	return strings.HasPrefix(e.msg, "timeout:")
}

func (e errWhere) Temporary() bool {
	// Is the error temporary?
	return true
}

// buffer provides a linked list buffer for data exchange
// between producer and consumer. Theoretically the buffer is
// of unlimited capacity as it does no allocation of its own.
type buffer struct {
	// protects concurrent access to head, tail and closed
	*sync.Cond

	head *element // the buffer that will be read first
	tail *element // the buffer that will be read last

	closed bool
	idle   *idleTimer
}

// An element represents a single link in a linked list.
type element struct {
	buf  []byte
	next *element
}

// newBuffer returns an empty buffer that is not closed.
func newBuffer(idle *idleTimer) *buffer {
	e := new(element)
	b := &buffer{
		Cond: newCond(),
		head: e,
		tail: e,
		idle: idle,
	}
	return b
}

// write makes buf available for Read to receive.
// buf must not be modified after the call to write.
func (b *buffer) write(buf []byte) {
	b.Cond.L.Lock()
	e := &element{buf: buf}
	b.tail.next = e
	b.tail = e
	b.Cond.Signal()
	b.Cond.L.Unlock()
}

// eof closes the buffer. Reads from the buffer once all
// the data has been consumed will receive os.EOF.
func (b *buffer) eof() error {
	b.Cond.L.Lock()
	//pp("buffer.eof is setting b.closed=true for b=%p. stack='%s'.", b, string(stacktrace()))
	b.closed = true
	b.Cond.Signal()
	b.Cond.L.Unlock()
	return nil
}

// timeout does not close the buffer. Reads from the buffer once all
// the data has been consumed will receive ErrTimeout.
// b.idle.TimeOut() must return true when queried for
// this to be succesful.
func (b *buffer) timeout() error {
	b.Cond.Signal()
	return nil
}

// Read reads data from the internal buffer in buf.  Reads will block
// if no data is available, or until the buffer is closed.
func (b *buffer) Read(buf []byte) (n int, err error) {
	b.Cond.L.Lock()
	defer func() {
		b.Cond.L.Unlock()
		if err == nil {
			b.idle.Reset()
		}
	}()

	//p("buffer.Read() on buf size %v", len(buf))

	for len(buf) > 0 {
		// if there is data in b.head, copy it
		if len(b.head.buf) > 0 {
			r := copy(buf, b.head.buf)
			buf, b.head.buf = buf[r:], b.head.buf[r:]
			n += r
			continue
		}
		// if there is a next buffer, make it the head
		if len(b.head.buf) == 0 && b.head != b.tail {
			b.head = b.head.next
			continue
		}

		// if at least one byte has been copied, return
		if n > 0 {
			break
		}

		// if nothing was read, and there is nothing outstanding
		// check to see if the buffer is closed.
		if b.closed {
			err = io.EOF
			break
		}
		timedOut := ""
		select {
		case timedOut = <-b.idle.TimedOut:
		case <-b.idle.halt.ReqStop.Chan:
		}
		if timedOut != "" {
			err = newErrTimeout(timedOut, b.idle)
			break
		}
		// out of buffers, wait for producer
		b.Cond.Wait()
	}
	return
}
