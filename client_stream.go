package h2client

import (
	"context"
	"errors"
	"net/http"
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
		//todo: write stream reset, forgetStreamByID
	}
}
