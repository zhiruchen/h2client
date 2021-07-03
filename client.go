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

	"golang.org/x/net/http2"
	"golang.org/x/net/http2/hpack"
)

var (
	clientPreface = []byte(http2.ClientPreface)
)

type ClientConn struct {
	conn *tls.Conn
	bw   *bufio.Writer
	br   *bufio.Reader
	fr   *http2.Framer

	werr error // first write error  occured

	hbuf bytes.Buffer
	henc *hpack.Encoder

	maxFrameSize uint32
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
		conn: tlsConn,
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

	// go cc.readLoop()

	streamID := cc.nextStreamID()
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
				StreamID:      streamID,
				BlockFragment: chunk,
				EndHeaders:    len(hdrsInBytes) == 0,
				EndStream:     !hasBody,
			})
			firstHeader = false
		} else {
			cc.fr.WriteContinuation(streamID, len(hdrsInBytes) == 0, chunk)
		}
	}

	cc.bw.Flush()
	if cc.werr != nil {
		return nil, cc.werr
	}
	// Server sends: HEADERS[+CONTINUATION]
	f, err = cc.fr.ReadFrame()
	if err != nil {
		return nil, err
	}
	fmt.Printf("Get frame after write headers: %v\n", f)

	return &http.Response{}, nil
}

func (cc *ClientConn) readLoop() {
	for {

	}
}

func (cc *ClientConn) encodeHeaders(req *http.Request) []byte {
	cc.hbuf.Reset()

	cc.writeHeader(":method", req.Method)
	cc.writeHeader(":scheme", "https")
	// cc.writeHeader(":authority", req.Host)
	cc.writeHeader(":path", req.URL.Path)

	for k, vs := range req.Header {
		for _, v := range vs {
			cc.writeHeader(strings.ToLower(k), v)
		}
	}

	return cc.hbuf.Bytes()
}

func (cc *ClientConn) writeHeader(name, value string) {
	cc.henc.WriteField(hpack.HeaderField{Name: name, Value: value})
}

func (cc *ClientConn) nextStreamID() uint32 {
	return 1
}
