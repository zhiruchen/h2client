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

func (p *pipe) CloseWithErr(err error) {}

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
