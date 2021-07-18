package h2client

import (
	"errors"
	"fmt"
	"math"
	"net/http"
	"strconv"

	"golang.org/x/net/http2"
)

var (
	errResponseHeaderExceedLimit = errors.New("http2: response header list larger than advertised limit")
	errMalformedResponse         = errors.New("[http2] malformed response from server: miss status header")
	errStatusNotNumeric          = errors.New("[http2] malformed response from server: status code is non-numeric")
)

type connReadLoop struct {
	cc *ClientConn
}

func (rl *connReadLoop) run() error {
	cc := rl.cc
	// gotReply := false
	getSettings := false

	for {
		f, err := cc.fr.ReadFrame()
		if err != nil {
			return err
		}

		fmt.Printf("Get frame: %#v\n", f)

		if !getSettings {
			if _, ok := f.(*http2.SettingsFrame); !ok {
				return fmt.Errorf("expect a settings frame")
			}

			getSettings = true
		}

		switch f := f.(type) {
		case *http2.MetaHeadersFrame:
			err = rl.processheaders(f)
		case *http2.DataFrame:
			err = rl.processData(f)
		case *http2.GoAwayFrame:
			err = rl.processgoAway(f)
		case *http2.RSTStreamFrame:
		case *http2.SettingsFrame:
			err = rl.processSettings(f)
		case *http2.WindowUpdateFrame:
		case *http2.PingFrame:
		default:
			fmt.Printf("[Transport] unhandled resp frame: %T\n", f)
		}

		if err != nil {
			fmt.Printf("[http2] connReadLoop error: %v\n", err)
			return err
		}
	}
}

func (rl *connReadLoop) processheaders(f *http2.MetaHeadersFrame) error {
	cc := rl.cc

	cs := cc.getStreamByID(f.StreamID)
	if cs == nil {
		return nil
	}

	if f.StreamEnded() {
		//todo: forget stream
	}

	res, err := rl.handleResponse(cs, f)
	if err != nil {
		if _, ok := err.(http2.ConnectionError); ok {
			return err
		}

		cs.resc <- h2Resp{err: err}
		return nil
	}
	if res == nil {
		return nil
	}

	cs.resc <- h2Resp{res: res}

	return nil
}

func (rl *connReadLoop) processData(f *http2.DataFrame) error {
	cc := rl.cc
	cs := cc.getStreamByID(f.StreamID)
	data := f.Data()
	if cs == nil {
		return nil
	}

	if f.Length > 0 {
		// received DATA on a HEAD request
		if cs.req.Method == "HEAD" && len(data) > 0 {
			rl.endStreamError(cs, http2.StreamError{
				StreamID: f.StreamID,
				Code:     http2.ErrCodeProtocol,
			})
			return nil
		}

		// todo: check connection level flow control
		if len(data) > 0 && !cs.didReset {
			if _, err := cs.bufPipe.Write(data); err != nil {
				rl.endStreamError(cs, err)
				return err
			}
		}
	}

	if f.StreamEnded() {
		rl.endStream(cs)
	}

	return nil
}

func (rl *connReadLoop) processgoAway(f *http2.GoAwayFrame) error {
	cc := rl.cc
	cc.t.ConnPool.MarkDead(cc)
	if f.ErrCode != 0 {
		fmt.Printf("[connReadLoop] got GoAway with error code = %v", f.ErrCode)
	}
	cc.setGoAway(f)
	return nil
}

func (rl *connReadLoop) processSettings(f *http2.SettingsFrame) error {
	cc := rl.cc
	cc.mu.Lock()
	defer cc.mu.Unlock()

	if f.IsAck() {
		if cc.wantSettingsAck {
			cc.wantSettingsAck = false
			return nil
		}

		return http2.ConnectionError(http2.ErrCodeProtocol)
	}

	err := f.ForeachSetting(func(s http2.Setting) error {
		switch s.ID {
		case http2.SettingMaxFrameSize:
			cc.maxFrameSize = s.Val
		case http2.SettingMaxConcurrentStreams:
			cc.maxConcurrentStreams = s.Val
		case http2.SettingMaxHeaderListSize:
			cc.peerMaxHeaderListSize = uint64(s.Val)
		case http2.SettingInitialWindowSize:
			if s.Val > math.MaxInt32 {
				return http2.ConnectionError(http2.ErrCodeFlowControl)
			}

			//todo: handle flow control

			cc.initialWindowSize = s.Val
		default:
			fmt.Printf("Unhandled setting: %v", s)
		}
		return nil
	})

	if err != nil {
		return err
	}

	cc.wmu.Lock()
	defer cc.wmu.Unlock()

	cc.fr.WriteSettingsAck()
	cc.bw.Flush()
	return cc.werr
}

func (rl *connReadLoop) handleResponse(cs *clientStream, f *http2.MetaHeadersFrame) (*http.Response, error) {
	if f.Truncated {
		return nil, errResponseHeaderExceedLimit
	}

	status := f.PseudoValue("status")
	if status == "" {
		return nil, errMalformedResponse
	}

	statusCode, err := strconv.Atoi(status)
	if err != nil {
		return nil, errStatusNotNumeric
	}

	header := make(http.Header)
	res := &http.Response{
		Proto:      "HTTP/2.0",
		ProtoMajor: 2,
		Header:     header,
		StatusCode: statusCode,
		Status:     status + " " + http.StatusText(statusCode),
	}

	for _, hf := range f.RegularFields() {
		key := http.CanonicalHeaderKey(hf.Name)
		if key == "Trailer" {
			//todo: handle res Trailer
			continue
		}

		header[key] = append(header[key], hf.Value)
	}

	streamEnded := f.StreamEnded()
	isHead := cs.req.Method == "HEAD"
	if !streamEnded || isHead {
		res.ContentLength = -1
		if ctnLen := res.Header["Content-Length"]; len(ctnLen) == 1 {
			if length, err := strconv.ParseInt(ctnLen[0], 10, 64); err != nil {
				res.ContentLength = length
			}
		}
	}

	if streamEnded || isHead {
		res.Body = http.NoBody
		return res, nil
	}

	cs.bufPipe = pipe{b: nil}
	cs.bytesRemain = res.ContentLength
	res.Body = resBody{cs: cs}

	go cs.awaitRequestCancel(cs.req)

	//todo: handle gzip
	return res, nil
}

func (rl *connReadLoop) endStream(cs *clientStream) {
	rl.endStreamError(cs, nil)
}

func (rl *connReadLoop) endStreamError(cs *clientStream, err error) {

}
