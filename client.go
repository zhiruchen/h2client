package h2client

import (
	"bufio"
	"bytes"
	"crypto/tls"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

var (
	clientPreface = []byte(http2.ClientPreface)
)

const (
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
	werr      error         // first write error  occured

	hbuf bytes.Buffer
	henc *hpack.Encoder
	hdec *hpack.Decoder

	respHeaders http.Header

	maxFrameSize          uint32
	initialWindowSize     uint32
	maxConcurrentStreams  uint32
	peerMaxHeaderListSize uint64

	mu           sync.Mutex
	streams      map[uint32]*clientStream
	nextStreamID uint32
}

type h2Resp struct {
	res *http.Response
	err error
}

type clientStream struct {
	cc  *ClientConn
	req *http.Request

	ID      uint32
	resc    chan h2Resp
	bufPipe pipe

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

	return resp, nil
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

func (t *Transport) newClientConn(conn net.Conn) (*ClientConn, error) {
	cc := &ClientConn{
		t:                     t,
		tconn:                 conn,
		nextStreamID:          1,
		maxFrameSize:          16 << 10,
		initialWindowSize:     65535,
		maxConcurrentStreams:  1000,
		peerMaxHeaderListSize: 0xffffffffffffffff,
		readDone:              make(chan struct{}),
		streams:               make(map[uint32]*clientStream),
	}

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
	cc.fr.WriteWindowUpdate(0, 1<<30)
	cc.bw.Flush()
	if cc.werr != nil {
		return nil, cc.werr
	}

	go cc.readLoop()
	return cc, nil
}

func (cc *ClientConn) onNewHeaderField(hf hpack.HeaderField) {
	fmt.Println("Header Field: ", hf)
	cc.respHeaders.Add(http.CanonicalHeaderKey(hf.Name), hf.Value)
}

func (cc *ClientConn) readLoop() {
	rl := &connReadLoop{cc: cc}
	cc.readerErr = rl.run()
}

func (cc *ClientConn) roundTrip(req *http.Request) (*http.Response, error) {

	cc.mu.Lock()
	// body := req.Body
	contentLength := req.ContentLength
	hasBody := contentLength != 0

	hdrs := cc.encodeHeaders(req)

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

func (cc *ClientConn) encodeHeaders(req *http.Request) []byte {
	cc.hbuf.Reset()
	var host = req.Host
	if host == "" {
		host = req.URL.Host
	}

	cc.writeHeader(":method", req.Method)
	cc.writeHeader(":scheme", "https")
	cc.writeHeader(":authority", host)
	cc.writeHeader(":path", req.URL.Path)

	for k, vs := range req.Header {
		for _, v := range vs {
			cc.writeHeader(strings.ToLower(k), v)
		}
	}

	if _, ok := req.Header[http.CanonicalHeaderKey("Host")]; !ok {
		cc.writeHeader("host", host)
	}

	return cc.hbuf.Bytes()
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
