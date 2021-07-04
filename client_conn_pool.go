package h2client

import (
	"net/http"
	"sync"
)

// ClientConnPool Manage pool of HTTP/2 client connection
type ClientConnPool interface {
	GetClientConn(req *http.Request, addr string) (*ClientConn, error)
}

type clientConnPool struct {
	t *Transport

	mu    sync.Mutex
	conns map[string][]*ClientConn // key: host:port
}

func (p *clientConnPool) GetClientConn(req *http.Request, addr string) (*ClientConn, error) {
	if req.Close {
		return p.t.dialClientConn(addr)
	}

	// todo: handle get client conn from pool
	return p.t.dialClientConn(addr)
}
