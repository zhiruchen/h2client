package h2client

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/http"

	"golang.org/x/net/http2"
)

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

	return &http.Response{}, nil
}
