package h2client

import (
	"context"
	"errors"
	"io"
	"net/http"

	"golang.org/x/net/http2"
)

var (
	errRequestCanceled = errors.New("request canceled")
)

func (cs *clientStream) awaitRequestCancel(req *http.Request) {
	if err := awaitRequestCancel(req, cs.done); err != nil {
		cs.cancelStream()
		cs.bufPipe.CloseWithErr(err)
	}
}

// waits for the user to cancel a request
func awaitRequestCancel(req *http.Request, done <-chan struct{}) error {
	ctx := reqContext(req)
	if req.Cancel == nil && ctx.Done() == nil {
		return nil
	}

	select {
	case <-req.Cancel:
		return errRequestCanceled
	case <-ctx.Done():
		return ctx.Err()
	case <-done:
		return nil
	}
}

func reqContext(req *http.Request) context.Context {
	return req.Context()
}

func (cs *clientStream) cancelStream() {
	cc := cs.cc
	cc.mu.Lock()
	didReset := cs.didReset
	cs.didReset = true
	cc.mu.Unlock()

	if !didReset {
		cc.writeStreamReset(cs.ID, http2.ErrCodeCancel, nil)
		cc.forgetStreamID(cs.ID)
	}
}

func (cs *clientStream) writeRequestBody(body io.Reader, bodyCloser io.Closer) (err error) {
	cc := cs.cc
	sentEnd := false
	buf := make([]byte, cc.maxFrameSize)

	defer func() {
		cerr := bodyCloser.Close()
		if err == nil {
			err = cerr
		}
	}()

	var bodyEOF bool
	for !bodyEOF {
		n, err := body.Read(buf)
		if err != nil && err != io.EOF {
			return err
		}

		if err == io.EOF {
			bodyEOF = true
			err = nil
		}

		remain := buf[:n]
		for len(remain) > 0 && err == nil {
			//todo: awaitFlowControl
			allowed := cc.maxFrameSize

			cc.wmu.Lock()
			data := remain[:allowed]
			remain = remain[allowed:]
			sentEnd = bodyEOF && len(remain) == 0
			err = cc.fr.WriteData(cs.ID, sentEnd, data)
			if err != nil {
				err = cc.bw.Flush()
			}
			cc.wmu.Unlock()
		}
		if err != nil {
			return err
		}
	}

	if sentEnd {
		return nil
	}

	cc.wmu.Lock()
	defer cc.wmu.Unlock()

	err = cc.fr.WriteData(cs.ID, true, nil)
	if ferr := cc.bw.Flush(); ferr != nil && err == nil {
		err = ferr
	}
	return err
}
