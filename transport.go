package h2client

import (
	"errors"
	"io"
	"net/http"
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

func (t *Transport) idleConnTimeout() time.Duration {
	if t.t1 != nil {
		return t.t1.IdleConnTimeout
	}

	return 0
}
