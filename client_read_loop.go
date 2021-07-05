package h2client

import (
	"fmt"

	"golang.org/x/net/http2"
)

type connReadLoop struct {
	cc *ClientConn
}

func (rl *connReadLoop) run() error {
	cc := rl.cc
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
		case *http2.DataFrame:
		case *http2.GoAwayFrame:
		case *http2.RSTStreamFrame:
		case *http2.SettingsFrame:
		case *http2.WindowUpdateFrame:
		case *http2.PingFrame:
		default:
			fmt.Printf("[Transport] unhandled resp frame: %T\n", f)
		}
	}
}
