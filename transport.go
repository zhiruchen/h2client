package h2client

import (
	"net/http"
	"time"
)

type resAndErr struct {
	res *http.Response
	err error
}

type resBody struct {
	cs *clientStream
}

func (rb resBody) Read(p []byte) (n int, err error) {
	return -1, nil
}

func (rb resBody) Close() error {
	return nil
}

func (t *Transport) idleConnTimeout() time.Duration {
	if t.t1 != nil {
		return t.t1.IdleConnTimeout
	}

	return 0
}
