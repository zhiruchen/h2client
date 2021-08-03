package h2client

import (
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"golang.org/x/net/http2"
)

var (
	errCloseResponseBody = errors.New("[http2] response body closed")
)

type resAndErr struct {
	res *http.Response
	err error
}

type resBody struct {
	cs *clientStream
}

func (b resBody) Read(p []byte) (n int, err error) {
	cs := b.cs
	cc := cs.cc
	if cs.readErr != nil {
		return 0, cs.readErr
	}

	n, err = b.cs.bufPipe.Read(p)
	if cs.bytesRemain != 1 {

	}
	if n == 0 {
		return
	}

	var connAdd, streamAdd int32
	if v := cc.inflow.available(); v < defaultTransportStreamFlow/2 {
		connAdd = defaultTransportStreamFlow - v
		cc.inflow.add(connAdd)
	}
	if err == nil {
		v := int(cs.inflow.available()) + cs.bufPipe.Len()
		if v < defaultTransportStreamFlow-defaultStreamMinRefresh {
			streamAdd = int32(defaultTransportStreamFlow - v)
			cs.inflow.add(streamAdd)
		}
	}

	if connAdd != 0 || streamAdd != 0 {
		cc.wmu.Lock()
		defer cc.wmu.Unlock()
		if connAdd != 0 {
			cc.fr.WriteWindowUpdate(0, uint32(connAdd))
		}
		if streamAdd != 0 {
			cc.fr.WriteWindowUpdate(cs.ID, uint32(streamAdd))
		}
		cc.bw.Flush()
	}
	return
}

func (b resBody) Close() error {
	cs := b.cs
	cc := cs.cc

	serverSentStreamEnd := cs.bufPipe.Err() == io.EOF
	unread := cs.bufPipe.Len()

	if unread > 0 || !serverSentStreamEnd {
		cc.mu.Lock()
		cc.wmu.Lock()
		if !serverSentStreamEnd {
			cc.fr.WriteRSTStream(cs.ID, http2.ErrCodeCancel)
			cs.didReset = true
		}

		if unread > 0 {
			cc.inflow.add(int32(unread))
			cc.fr.WriteWindowUpdate(0, uint32(unread))
		}
		cc.bw.Flush()
		cc.wmu.Unlock()
		cc.mu.Unlock()
	}
	cs.bufPipe.BreakWithError(errCloseResponseBody)
	cc.forgetStreamID(cs.ID)
	return nil
}

func (t *Transport) RoundTripOpt(req *http.Request, opt http2.RoundTripOpt) (*http.Response, error) {
	if !(req.URL.Scheme == "https" || (req.URL.Scheme == "http" && t.AllowHTTP)) {
		return nil, errors.New("[http2] unsuuprted scheme")
	}

	addr := authorityAddr(req.URL.Scheme, req.URL.Host)
	cc, err := t.connPool().GetClientConn(req, addr)
	if err != nil {
		return nil, err
	}

	res, _, err := cc.roundTrip(req)
	if err != nil {
		return nil, err
	}

	return res, nil
}

func (t *Transport) idleConnTimeout() time.Duration {
	if t.t1 != nil {
		return t.t1.IdleConnTimeout
	}

	return 0
}

type bodyWriterSate struct {
	cs     *clientStream
	resc   chan error
	fn     func()
	fnonce *sync.Once
	timer  *time.Timer
	delay  time.Duration
}

func (t *Transport) getBodyWriterState(cs *clientStream, body io.Reader) (s bodyWriterSate) {
	s.cs = cs
	if body == nil {
		return
	}

	resc := make(chan error, 1)
	s.resc = resc
	s.fn = func() {
		cs.cc.mu.Lock()
		cs.startedWriteBody = true
		cs.cc.mu.Unlock()
		resc <- cs.writeRequestBody(body, cs.req.Body)
	}
	s.delay = t.expectContinueTimeout()

	s.fnonce = new(sync.Once)
	s.timer = time.AfterFunc(365*24*time.Hour, func() {
		s.fnonce.Do(s.fn)
	})
	return
}

func (s bodyWriterSate) cancel() {
	if s.timer != nil {
		s.timer.Stop()
	}
}

func (s bodyWriterSate) scheduleBodyWrite() {
	if s.timer == nil {
		go s.fn()
		return
	}

	if s.timer.Stop() {
		s.timer.Reset(s.delay)
	}
}
