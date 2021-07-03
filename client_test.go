package h2client

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRoundTrip(t *testing.T) {
	tr := &Transport{}
	req, err := http.NewRequest("GET", "https://httpbin.org/get", nil)
	assert.Nil(t, err)

	resp, err := tr.RoundTrip(req)
	assert.Nil(t, err)
	if assert.NotNil(t, resp) {
		fmt.Printf("resp: %+v\n", *resp)
		defer resp.Body.Close()
		bs, err := ioutil.ReadAll(resp.Body)
		if err != nil {
			fmt.Printf("resp body err: %v\n", err)
		} else {
			fmt.Printf("resp body: %s\n", string(bs))
		}
	}
}
