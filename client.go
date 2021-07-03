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

	maxFrameSize uint32
	mu           sync.Mutex
	streams      map[uint32]*clientStream
	nextStreamID uint32
}

type clientStream struct {
	ID   uint32
	resc chan *http.Response
	pw   *io.PipeWriter
	pr   *io.PipeReader
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
	Fallback http.RoundTripper
}

func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.Scheme != "https" {
		if t.Fallback == nil {
			return nil, fmt.Errorf("http2: unsupported scheme and no Fallback")
		}
		return t.Fallback.RoundTrip(req)
	}

	host, port, err := net.SplitHostPort(req.URL.Host)
	if err != nil {
		host = req.URL.Host
		port = "443"
	}

	tlsConfig := &tls.Config{
		ServerName: host,
		NextProtos: []string{http2.NextProtoTLS},
	}
	tlsConn, err := tls.Dial("tcp", fmt.Sprintf("%s:%s", host, port), tlsConfig)
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
	if _, err = tlsConn.Write(clientPreface); err != nil {
		return nil, err
	}

	cc := &ClientConn{
		conn:         tlsConn,
		readDone:     make(chan struct{}),
		streams:      make(map[uint32]*clientStream),
		nextStreamID: 1,
	}
	cc.bw = bufio.NewWriter(stickyErrWriter{tlsConn, &cc.werr})
	cc.br = bufio.NewReader(cc.conn)
	cc.henc = hpack.NewEncoder(&cc.hbuf)
	cc.fr = http2.NewFramer(cc.bw, cc.br)

	cc.fr.WriteSettings()
	cc.bw.Flush()
	if cc.werr != nil {
		return nil, cc.werr
	}

	f, err := cc.fr.ReadFrame()
	if err != nil {
		return nil, err
	}
	fmt.Printf("Get frame: %#v\n", f)

	sf, ok := f.(*http2.SettingsFrame)
	if !ok {
		return nil, fmt.Errorf("expect a settings frame")
	}
	cc.fr.WriteSettingsAck()
	cc.bw.Flush()

	//Todo:
	/*
		Setting frame: [INITIAL_WINDOW_SIZE = 67108864]
		Setting frame: [MAX_CONCURRENT_STREAMS = 100]
		Setting frame: [MAX_FRAME_SIZE = 65536]
	*/
	sf.ForeachSetting(func(s http2.Setting) error {
		fmt.Printf("Setting frame: %v\n", s)
		switch s.ID {
		case http2.SettingMaxFrameSize:
			cc.maxFrameSize = s.Val
		default:
			fmt.Printf("Unhandled setting: %v", s)
		}
		return nil
	})

	cc.hdec = hpack.NewDecoder(initialHeaderTableSize, cc.onNewHeaderField)

	go cc.readLoop()

	cs := cc.newStream()
	hasBody := false

	// Send Headers[+CONTINUATION] + (DATA?)
	hdrsInBytes := cc.encodeHeaders(req)
	firstHeader := true
	for len(hdrsInBytes) > 0 {
		chunk := hdrsInBytes
		if len(chunk) > int(cc.maxFrameSize) {
			chunk = chunk[:cc.maxFrameSize]
		}

		hdrsInBytes = hdrsInBytes[len(chunk):]
		if firstHeader {
			cc.fr.WriteHeaders(http2.HeadersFrameParam{
				StreamID:      cs.ID,
				BlockFragment: chunk,
				EndHeaders:    len(hdrsInBytes) == 0,
				EndStream:     !hasBody,
			})
			firstHeader = false
		} else {
			cc.fr.WriteContinuation(cs.ID, len(hdrsInBytes) == 0, chunk)
		}
	}

	cc.bw.Flush()
	if cc.werr != nil {
		return nil, cc.werr
	}

	resp := <-cs.resc
	return resp, nil
}

func (cc *ClientConn) onNewHeaderField(hf hpack.HeaderField) {
	fmt.Println("Header Field: ", hf)
	cc.respHeaders.Add(http.CanonicalHeaderKey(hf.Name), hf.Value)
}

func (cc *ClientConn) readLoop() {
	defer close(cc.readDone)
	for {
		f, err := cc.fr.ReadFrame()
		if err != nil {
			fmt.Println("readFrame error: ", err)
			cc.readerErr = err
			return
		}

		fmt.Printf("Read %v: %v\n", f.Header(), f)
		cs := cc.getStreamByID(f.Header().StreamID)

		headerEnd := false
		streamEnd := false

		switch f := f.(type) {
		case *http2.HeadersFrame:
			cc.respHeaders = make(http.Header)
			cs.pr, cs.pw = io.Pipe()

			cc.hdec.Write(f.HeaderBlockFragment())
			headerEnd = f.HeadersEnded()
			streamEnd = f.StreamEnded()

		case *http2.ContinuationFrame:
			cc.hdec.Write(f.HeaderBlockFragment())
			headerEnd = f.HeadersEnded()
			streamEnd = f.Header().Flags.Has(http2.FlagHeadersEndStream)

		case *http2.DataFrame:
			fmt.Printf("data: %v\n", f.Data())
			if cs != nil {
				cs.pw.Write(f.Data())
			}
		}

		if streamEnd {
			cs.pw.Close()
		}

		if headerEnd {
			if cs == nil {
				panic("stream not found")
			}

			cs.resc <- &http.Response{
				Header: cc.respHeaders,
				Body:   cs.pr,
			}
		}
	}
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

func (cc *ClientConn) writeHeader(name, value string) {
	cc.henc.WriteField(hpack.HeaderField{Name: name, Value: value})
}

func (cc *ClientConn) newStream() *clientStream {
	cc.mu.Lock()
	defer cc.mu.Unlock()

	cs := &clientStream{
		ID:   cc.nextStreamID,
		resc: make(chan *http.Response, 1),
	}
	cc.streams[cs.ID] = cs
	cc.nextStreamID += 2
	return cs
}

func (cc *ClientConn) getStreamByID(id uint32) *clientStream {
	cc.mu.Lock()
	defer cc.mu.Unlock()
	return cc.streams[id]
}
