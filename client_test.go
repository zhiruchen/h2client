package h2client

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestRoundTrip(t *testing.T) {
	tr := &Transport{}
	req, err := http.NewRequest("GET", "https://github.com/zhiruchen", nil)
	assert.Nil(t, err)

	_, err = tr.RoundTrip(req)
	assert.Nil(t, err)
}
