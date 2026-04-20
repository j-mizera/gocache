package resp

import "sync"

// Pool sizing. These caps prevent rare huge buffers (e.g. a 10 MB LRANGE reply
// or a 64 MB bulk-string SET) from being retained indefinitely by idle
// goroutines. Buffers over the cap are released to the GC instead of pooled.
const (
	// maxPooledScratchCap bounds the write-path scratch buffer we keep in the
	// pool. 512 KiB covers LRANGE of ~5–10k items with realistic value sizes;
	// above this the workload is far from typical and re-allocation is
	// cheaper than per-worker memory bloat.
	maxPooledScratchCap = 512 * 1024

	// maxPooledBulkCap bounds the read-path bulk-string scratch buffer. 512
	// KiB keeps the pool useful for values up to that size; gigantic bulk
	// writes (SET with a multi-MB payload) fall through to one-off allocation.
	maxPooledBulkCap = 512 * 1024

	// initialScratchCap is the starting capacity for a freshly-minted scratch
	// buffer. Sized for typical GET/SET replies.
	initialScratchCap = 256
)

// scratchBufPool recycles []byte used by Writer.Write to assemble a RESP frame
// before a single bufio.Write. The pool holds *[]byte (pointer-to-slice) per
// the sync.Pool idiom — putting a bare slice would allocate a fresh slice
// header on every Put.
var scratchBufPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, initialScratchCap)
		return &b
	},
}

func getScratch() *[]byte {
	p := scratchBufPool.Get().(*[]byte)
	*p = (*p)[:0]
	return p
}

func putScratch(p *[]byte) {
	if cap(*p) > maxPooledScratchCap {
		return
	}
	scratchBufPool.Put(p)
}

// bulkScratchPool recycles []byte used by Reader.readBulkString /
// Reader.readBulkError to hold the raw payload before string-conversion. The
// conversion itself still allocates one string — that's unavoidable because
// Value.Str is a string — but the scratch []byte is eliminated from the per
// request allocation count.
var bulkScratchPool = sync.Pool{
	New: func() any {
		b := make([]byte, 0, initialScratchCap)
		return &b
	},
}

func getBulkScratch(n int) *[]byte {
	p := bulkScratchPool.Get().(*[]byte)
	buf := *p
	if cap(buf) < n {
		buf = make([]byte, n)
	} else {
		buf = buf[:n]
	}
	*p = buf
	return p
}

func putBulkScratch(p *[]byte) {
	if cap(*p) > maxPooledBulkCap {
		return
	}
	*p = (*p)[:0]
	bulkScratchPool.Put(p)
}
