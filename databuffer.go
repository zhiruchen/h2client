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

func putDataBufferChunk(p []byte) {
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

func (b *dataBuffer) Len() int {
	return b.size
}

func (b *dataBuffer) Read(p []byte) (int, error) {
	if b.size == 0 {
		return 0, fmt.Errorf("dataBuffer is empty")
	}

	var ntotal int
	for len(p) > 0 && b.size > 0 {
		first := b.bytesFromFirstChunk()
		n := copy(p, first)
		p = p[n:]

		ntotal += n
		b.r += n
		b.size -= n

		if b.r == len(b.chunks[0]) {
			putDataBufferChunk(b.chunks[0])
			end := len(b.chunks) - 1
			copy(b.chunks[:end], b.chunks[1:])
			b.chunks[end] = nil
			b.chunks = b.chunks[:end]
			b.r = 0
		}
	}
	return ntotal, nil
}

func (b *dataBuffer) bytesFromFirstChunk() []byte {
	if len(b.chunks) == 1 {
		return b.chunks[0][b.r:b.w]
	}

	return b.chunks[0][b.r:]
}

func (b *dataBuffer) Write(p []byte) (int, error) {
	ntotal := len(p)
	for len(p) > 0 {
		want := int64(len(p))
		if b.expected > want {
			want = b.expected
		}

		lastChunk := b.lastChunkOrAlloc(want)
		n := copy(lastChunk[b.w:], p)
		p = p[n:]
		b.w += n
		b.size += n
		b.expected -= int64(n)
	}

	return ntotal, nil
}

func (b *dataBuffer) lastChunkOrAlloc(size int64) []byte {
	if len(b.chunks) != 0 {
		last := b.chunks[len(b.chunks)-1]
		if b.w < len(last) {
			return last
		}
	}

	chunk := getDataBufferChunk(size)
	b.chunks = append(b.chunks, chunk)
	b.w = 0
	return chunk
}
