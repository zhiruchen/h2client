package h2client

import (
	"crypto/tls"
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

	mu           sync.Mutex
	conns        map[string][]*ClientConn // key: host:port
	keys         map[*ClientConn][]string
	addConnCalls map[string]*addConnCall // in-flight addConnIfNeed calls
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

func (p *clientConnPool) addConnIfNeed(key string, t *Transport, conn *tls.Conn) (bool, error) {
	p.mu.Lock()
	for _, cc := range p.conns[key] {
		if cc.CanTakeNewRequest() {
			p.mu.Unlock()
			return false, nil
		}
	}

	call, ok := p.addConnCalls[key]
	if !ok {
		if p.addConnCalls == nil {
			p.addConnCalls = make(map[string]*addConnCall)
		}

		call = &addConnCall{
			p:    p,
			done: make(chan struct{}),
		}
		p.addConnCalls[key] = call
		go call.run(t, key, conn)
	}
	p.mu.Unlock()

	<-call.done
	if call.err != nil {
		return false, call.err
	}

	return !ok, nil
}

type addConnCall struct {
	p    *clientConnPool
	done chan struct{}
	err  error
}

func (c *addConnCall) run(t *Transport, key string, tc *tls.Conn) {
	cc, err := t.NewClientConn(tc)

	p := c.p
	p.mu.Lock()
	if err != nil {
		c.err = err
	} else {
		p.addConnLocked(key, cc)
	}

	delete(p.addConnCalls, key)
	p.mu.Unlock()
	close(c.done)
}

func (p *clientConnPool) addConn(key string, cc *ClientConn) {
	p.mu.Lock()
	p.addConnLocked(key, cc)
	p.mu.Unlock()
}

func (p *clientConnPool) addConnLocked(key string, cc *ClientConn) {
	for _, v := range p.conns[key] {
		if v == cc {
			return
		}
	}

	if p.conns == nil {
		p.conns = make(map[string][]*ClientConn)
	}

	if p.keys == nil {
		p.keys = make(map[*ClientConn][]string)
	}
	p.conns[key] = append(p.conns[key], cc)
	p.keys[cc] = append(p.keys[cc], key)
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
