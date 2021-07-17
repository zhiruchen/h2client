package h2client

import (
	"net/http"
	"sync"
)

// ClientConnPool Manage pool of HTTP/2 client connection
type ClientConnPool interface {
	GetClientConn(req *http.Request, addr string) (*ClientConn, error)
	MarkDead(*ClientConn)
}

type clientConnPool struct {
	t *Transport

	mu    sync.Mutex
	conns map[string][]*ClientConn // key: host:port
	keys  map[*ClientConn][]string
}

func (p *clientConnPool) GetClientConn(req *http.Request, addr string) (*ClientConn, error) {
	if req.Close {
		return p.t.dialClientConn(addr)
	}

	// todo: handle get client conn from pool
	return p.t.dialClientConn(addr)
}

func (p *clientConnPool) MarkDead(cc *ClientConn) {
	p.mu.Lock()
	defer p.mu.Unlock()

	for _, key := range p.keys[cc] {
		vv, ok := p.conns[key]
		if !ok {
			continue
		}

		newList := filterOutClientConn(vv, cc)
		if len(newList) > 0 {
			p.conns[key] = newList
		} else {
			delete(p.conns, key)
		}
	}

	delete(p.keys, cc)
}

func filterOutClientConn(conns []*ClientConn, exclude *ClientConn) []*ClientConn {
	out := conns[:0]
	for _, v := range conns {
		if v != exclude {
			out = append(out, v)
		}
	}

	// filtered it out, zero out the last item to prevent the GC from seeing it
	if len(conns) != len(out) {
		conns[len(conns)-1] = nil
	}

	return out
}
