package h2client

import (
	"fmt"
	"io"
	"sync"
)

type pipeBuffer interface {
	Len() int
	io.Writer
	io.Reader
}

type pipe struct {
	mu       sync.Mutex
	c        sync.Cond
	b        pipeBuffer
	err      error         // read error
	breakErr error         // immediate read error
	done     chan struct{} // close when error
	readFn   func()
}

func (p *pipe) Len() int {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.b == nil {
		return 0
	}
	return p.b.Len()
}

// Read copy bytes from the buffer into d
func (p *pipe) Read(d []byte) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.c.L == nil {
		p.c.L = &p.mu
	}
	for {
		if p.breakErr != nil {
			return 0, p.breakErr
		}
		if p.b != nil && p.b.Len() > 0 {
			return p.b.Read(d)
		}
		if p.err != nil {
			if p.readFn != nil {
				p.readFn()
				p.readFn = nil
			}

			p.b = nil
			return 0, p.err
		}
		p.c.Wait()
	}
}

// CloseWithErr cause the next read return the provided error, after all data has been read
func (p *pipe) CloseWithErr(err error) {
	p.closeWithError(&p.err, err, nil)
}

// BreakWithError cause the next read return the provided error immediately, without wait the unread data
func (p *pipe) BreakWithError(err error) {
	p.closeWithError(&p.breakErr, err, nil)
}

func (p *pipe) closeWithErrAndCode(err error, readFn func()) {
	p.closeWithError(&p.err, err, readFn)
}

func (p *pipe) closeWithError(dst *error, err error, readFn func()) {
	if err == nil {
		panic("err can not be nil")
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.c.L == nil {
		p.c.L = &p.mu
	}
	defer p.c.Signal()
	if *dst != nil {
		return
	}

	p.readFn = readFn
	if dst == &p.breakErr {
		p.b = nil
	}
	*dst = err
	p.closeDoneLocked()
}

func (p *pipe) closeDoneLocked() {
	if p.done == nil {
		return
	}

	select {
	case <-p.done:
	default:
		close(p.done)
	}
}

func (p *pipe) Write(d []byte) (n int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.c.L == nil {
		p.c.L = &p.mu
	}
	defer p.c.Signal()
	if p.err != nil {
		return 0, fmt.Errorf("write on closed buffer")
	}
	if p.breakErr != nil {
		return len(d), nil
	}

	return p.b.Write(d)
}

func (p *pipe) Err() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.breakErr != nil {
		return p.breakErr
	}
	return p.err
}

func (p *pipe) Done() <-chan struct{} {
	p.mu.Lock()
	defer p.mu.Lock()

	if p.done == nil {
		p.done = make(chan struct{})
		if p.breakErr != nil || p.err != nil {
			p.closeDoneLocked()
		}
	}
	return p.done
}
