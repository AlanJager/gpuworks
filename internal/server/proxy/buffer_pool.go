package proxy

import (
	"sync"
)

const proxyBufferSize = 32 << 10

type bufferPool struct{ p sync.Pool }

func newBufferPool() *bufferPool {
	bp := &bufferPool{}
	bp.p.New = func() any { b := make([]byte, proxyBufferSize); return &b }
	return bp
}

func (bp *bufferPool) Get() []byte {
	if bp == nil {
		return make([]byte, proxyBufferSize)
	}
	return *bp.p.Get().(*[]byte)
}

func (bp *bufferPool) Put(b []byte) {
	if bp == nil || cap(b) != proxyBufferSize {
		return
	}
	b = b[:proxyBufferSize]
	bp.p.Put(&b)
}
