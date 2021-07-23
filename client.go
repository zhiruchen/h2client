package h2client

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/net/http/httpguts"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

var (
	clientPreface = []byte(http2.ClientPreface)

	errClientConnGotGoAway = errors.New("[http2] transport received server's GoAway")
)

const (
	defaultConnFlow        = 1 << 30
	initialWindowSize      = 65535
	initialHeaderTableSize = 4096
)

type ClientConn struct {
	t        *Transport
	tconn    net.Conn
	tlsState *tls.ConnectionState

	conn *tls.Conn
	bw   *bufio.Writer
	br   *bufio.Reader
	fr   *http2.Framer

	readDone  chan struct{} // close on error
	readerErr error         // set after readDone is closed

	hbuf bytes.Buffer
	henc *hpack.Encoder
	hdec *hpack.Decoder

	respHeaders http.Header

	maxFrameSize          uint32
	initialWindowSize     uint32
	maxConcurrentStreams  uint32
	peerMaxHeaderListSize uint64

	mu     sync.Mutex
	cond   *sync.Cond
	flow   flow
	inflow flow

	wantSettingsAck bool // client send settings frame, have not ack frame
	goAway          *http2.GoAwayFrame
	goAwayDebug     string
	streams         map[uint32]*clientStream
	nextStreamID    uint32
	pings           map[[8]byte]chan struct{}

	wmu  sync.Mutex
	werr error // first write error  occured
}

type h2Resp struct {
	res *http.Response
	err error
}

type clientStream struct {
	cc  *ClientConn
	req *http.Request

	ID          uint32
	resc        chan h2Resp
	bufPipe     pipe
	flow        flow
	bytesRemain int64
	didReset    bool

	peerReset  chan struct{}
	resetError error

	done chan struct{}

	pw *io.PipeWriter
	pr *io.PipeReader
}

type stickyErrWriter struct {
	w   io.Writer
	err *error
}

func (se stickyErrWriter) Write(p []byte) (int, error) {
	if *se.err != nil {
		return 0, *se.err
	}

	n, err := se.w.Write(p)
	*se.err = err
	return n, err
}

type Transport struct {
	TLSClientConfig *tls.Config
	ConnPool        ClientConnPool

	// How many bytes of the response headers are allowed
	MaxHeaderListSize uint32
	t1                http.RoundTripper
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		if t.t1 == nil {
			return nil, fmt.Errorf("http2: unsupported scheme and no Fallback")
		}
		return t.t1.RoundTrip(req)
	}

	if t.ConnPool == nil {
		t.ConnPool = &clientConnPool{}
	}

	addr := authorityAddr(req.URL.Scheme, req.URL.Host)
	cc, err := t.ConnPool.GetClientConn(req, addr)
	if err != nil {
		return nil, err
	}
	fmt.Printf("get cc: %+v\n", cc)

	return &http.Response{}, nil
}

func authorityAddr(scheme string, authority string) (addr string) {
	host, port, err := net.SplitHostPort(authority)
	if err != nil {
		port = "443"
		if scheme == "http" {
			port = "80"
		}

		host = authority
	}

	return net.JoinHostPort(host, port)
}

func (t *Transport) dialClientConn(addr string) (*ClientConn, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}

	tlsConfig := &tls.Config{
		ServerName: host,
		NextProtos: []string{http2.NextProtoTLS},
	}
	tlsConn, err := tls.Dial("tcp", addr, tlsConfig)
	if err != nil {
		return nil, err
	}

	if err := tlsConn.Handshake(); err != nil {
		return nil, err
	}
	if err := tlsConn.VerifyHostname(tlsConfig.ServerName); err != nil {
		return nil, err
	}

	state := tlsConn.ConnectionState()
	fmt.Printf("conn state: %+v\n", state)
	if p := state.NegotiatedProtocol; p != http2.NextProtoTLS {
		return nil, fmt.Errorf("bad protocol: %v", p)
	}

	return t.newClientConn(tlsConn)
}

func (t *Transport) NewClientConn(conn net.Conn) (*ClientConn, error) {
	return t.newClientConn(conn)
}

func (t *Transport) newClientConn(conn net.Conn) (*ClientConn, error) {
	cc := &ClientConn{
		t:                     t,
		tconn:                 conn,
		readDone:              make(chan struct{}),
		nextStreamID:          1,
		maxFrameSize:          16 << 10,
		initialWindowSize:     initialWindowSize,
		maxConcurrentStreams:  1000,
		peerMaxHeaderListSize: 0xffffffffffffffff,
		streams:               make(map[uint32]*clientStream),
		wantSettingsAck:       true,
		pings:                 make(map[[8]byte]chan struct{}),
	}

	cc.cond = sync.NewCond(&cc.mu)
	cc.flow.add(int32(cc.initialWindowSize))

	cc.bw = bufio.NewWriter(stickyErrWriter{conn, &cc.werr})
	cc.br = bufio.NewReader(cc.conn)
	cc.fr = http2.NewFramer(cc.bw, cc.br)
	cc.fr.ReadMetaHeaders = hpack.NewDecoder(initialHeaderTableSize, nil)
	cc.fr.MaxHeaderListSize = t.MaxHeaderListSize

	cc.henc = hpack.NewEncoder(&cc.hbuf)

	initialSettings := []http2.Setting{
		{ID: http2.SettingEnablePush, Val: 0},
		{ID: http2.SettingInitialWindowSize, Val: 4 << 20},
		{ID: http2.SettingMaxHeaderListSize, Val: t.MaxHeaderListSize},
	}

	cc.bw.Write(clientPreface)
	cc.fr.WriteSettings(initialSettings...)
	cc.fr.WriteWindowUpdate(0, defaultConnFlow)
	cc.inflow.add(defaultConnFlow + initialWindowSize)
	cc.bw.Flush()
	if cc.werr != nil {
		return nil, cc.werr
	}

	go cc.readLoop()
	return cc, nil
}

func (cc *ClientConn) setGoAway(f *http2.GoAwayFrame) {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	old := cc.goAway
	cc.goAway = f

	if cc.goAwayDebug == "" {
		cc.goAwayDebug = string(f.DebugData())
	}

	if old != nil && old.ErrCode != http2.ErrCodeNo {
		cc.goAway.ErrCode = old.ErrCode
	}

	last := f.LastStreamID
	for streamID, cs := range cc.streams {
		if streamID > last {
			select {
			case cs.resc <- h2Resp{err: errClientConnGotGoAway}:
			default:
			}
		}
	}
}

func (cc *ClientConn) CanTakeNewRequest() bool {
	return false
}

func (cc *ClientConn) onNewHeaderField(hf hpack.HeaderField) {
	fmt.Println("Header Field: ", hf)
	cc.respHeaders.Add(http.CanonicalHeaderKey(hf.Name), hf.Value)
}

func (cc *ClientConn) readLoop() {
	rl := &connReadLoop{cc: cc}
	cc.readerErr = rl.run()
	if ce, ok := cc.readerErr.(http2.ConnectionError); ok {
		cc.wmu.Lock()
		cc.fr.WriteGoAway(0, http2.ErrCode(ce), nil)
		cc.mu.Unlock()
	}
}

func (cc *ClientConn) RoundTrip(req *http.Request) (*http.Response, error) {
	return cc.roundTrip(req)
}

func (cc *ClientConn) roundTrip(req *http.Request) (*http.Response, error) {
	cc.mu.Lock()
	// body := req.Body
	contentLength := req.ContentLength
	hasBody := contentLength != 0

	hdrs, err := cc.encodeHeaders(req, contentLength)
	if err != nil {
		cc.mu.Unlock()
		return nil, err
	}

	cs := cc.newStream()
	cs.req = req
	endStream := !hasBody
	werr := cc.writeHeaders(cs.ID, endStream, int(cc.maxFrameSize), hdrs)
	cc.mu.Unlock()

	if werr != nil {
		fmt.Printf("[roundTrip] writeHeaders error: %v\n", werr)
	}

	res := <-cs.resc
	return res.res, nil
}

func (cc *ClientConn) encodeHeaders(req *http.Request, contentLength int64) ([]byte, error) {
	cc.hbuf.Reset()
	var host = req.Host
	if host == "" {
		host = req.URL.Host
	}

	host, err := httpguts.PunycodeHostPort(host)
	if err != nil {
		return nil, err
	}

	var path string
	if req.Method != "CONNECT" {
		path = req.URL.RequestURI()
		if !validPseudoPath(path) {
			orig := path
			path = strings.TrimPrefix(path, req.URL.Scheme+"://"+host)
			if !validPseudoPath(path) {
				if req.URL.Opaque != "" {
					return nil, fmt.Errorf("invalid request :path %q from URL.Opaque = %q", orig, req.URL.Opaque)
				} else {
					return nil, fmt.Errorf("invalid request :path %q", orig)
				}
			}
		}
	}

	for header, vals := range req.Header {
		if !httpguts.ValidHeaderFieldName(header) {
			return nil, fmt.Errorf("invalid HTTP header name %q", header)
		}
		for _, val := range vals {
			if !httpguts.ValidHeaderFieldValue(val) {
				return nil, fmt.Errorf("invalid HTTP header value %q for header %q", val, header)
			}
		}
	}

	cc.writeHeader(":authority", host)
	cc.writeHeader(":method", req.Method)
	cc.writeHeader(":scheme", "https")

	if req.Method != "CONNECT" {
		cc.writeHeader(":path", path)
		cc.writeHeader(":scheme", req.URL.Scheme)
	}

	if shouldSendReqContentLength(req.Method, contentLength) {
		cc.writeHeader("content-length", strconv.FormatInt(contentLength, 10))
	}

	for k, headers := range req.Header {
		for _, v := range headers {
			cc.writeHeader(strings.ToLower(k), v)
		}
	}

	return cc.hbuf.Bytes(), nil
}

func shouldSendReqContentLength(method string, contentLength int64) bool {
	if contentLength > 0 {
		return true
	}

	if contentLength < 0 {
		return false
	}

	switch method {
	case "POST", "PUT", "PATCH":
		return true
	default:
		return false
	}
}

// a valid :path pseudo-header is
// 1) non-empty string start with '/
// 2) the string '*', for OPTIONS request
func validPseudoPath(path string) bool {
	return (len(path) > 0 && path[0] == '/') || path == "*"
}

func (cc *ClientConn) writeHeaders(streamID uint32, endStream bool, maxFrameSize int, hdrs []byte) error {
	first := false // first frame written

	for len(hdrs) > 0 && cc.werr == nil {
		chunk := hdrs
		if len(chunk) > maxFrameSize {
			chunk = chunk[:maxFrameSize]
		}

		hdrs = hdrs[len(chunk):]
		endHeaders := len(hdrs) == 0

		if first {
			cc.fr.WriteHeaders(http2.HeadersFrameParam{
				StreamID:      streamID,
				BlockFragment: chunk,
				EndStream:     endStream,
				EndHeaders:    endHeaders,
			})

			first = false
			continue
		}

		cc.fr.WriteContinuation(streamID, endHeaders, chunk)
	}

	cc.bw.Flush()
	return cc.werr
}

func (cc *ClientConn) writeHeader(name, value string) {
	cc.henc.WriteField(hpack.HeaderField{Name: name, Value: value})
}

func (cc *ClientConn) newStream() *clientStream {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	cs := &clientStream{
		cc:   cc,
		ID:   cc.nextStreamID,
		resc: make(chan h2Resp, 1),
		done: make(chan struct{}),
	}
	cc.nextStreamID += 2
	cc.streams[cs.ID] = cs
	return cs
}

func (cc *ClientConn) getStreamByID(id uint32) *clientStream {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.streams[id]
}
