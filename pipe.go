package h2client

import (
	"io"
	"sync"
)

type pipeBuffer interface {
	Len() int
	io.Writer
	io.Reader
}

type pipe struct {
	mu sync.Mutex
	b  pipeBuffer
}

func (p *pipe) CloseWithErr(err error) {}

func (p *pipe) Write(d []byte) (n int, err error) {
	return -1, nil
}
