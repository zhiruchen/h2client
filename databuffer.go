package h2client

import (
	"fmt"
	"sync"
)

var (
	dataChunkSizeClasses = []int{
		1 << 10,
		2 << 10,
		4 << 10,
		8 << 10,
		16 << 10,
	}

	dataChunkPools = [...]sync.Pool{
		{New: func() interface{} { return make([]byte, 1<<10) }},
		{New: func() interface{} { return make([]byte, 2<<10) }},
		{New: func() interface{} { return make([]byte, 4<<10) }},
		{New: func() interface{} { return make([]byte, 8<<10) }},
		{New: func() interface{} { return make([]byte, 16<<10) }},
	}
)

func getDataBufferChunk(size int64) []byte {
	i := 0
	for ; i < len(dataChunkSizeClasses)-1; i++ {
		if size <= int64(dataChunkSizeClasses[i]) {
			break
		}
	}

	return dataChunkPools[i].Get().([]byte)
}

func putDataBufferchunk(p []byte) {
	for i, n := range dataChunkSizeClasses {
		if len(p) == n {
			dataChunkPools[i].Put(p)
			return
		}
	}
	panic(fmt.Sprintf("unexpected buffer len=%d", len(p)))
}

type dataBuffer struct {
	chunks   [][]byte
	r        int   // next byte read index: chunks[0][r]
	w        int   // next byte to write is chunks[len(chunks)-1][w]
	size     int   // total buffered bytes
	expected int64 // expect at least expected Length bytes for write calls
}
