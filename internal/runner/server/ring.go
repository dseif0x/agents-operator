package server

import "sync"

// Ring is a fixed-size byte ring buffer that keeps the most recent bytes
// written to it. It is safe for concurrent use.
type Ring struct {
	mu    sync.Mutex
	buf   []byte
	start int // index of the oldest byte
	n     int // number of valid bytes
}

// NewRing returns a ring buffer that retains at most size bytes.
func NewRing(size int) *Ring {
	if size <= 0 {
		size = 1
	}
	return &Ring{buf: make([]byte, size)}
}

// Write appends p, dropping the oldest bytes when the buffer is full.
func (r *Ring) Write(p []byte) (int, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	size := len(r.buf)
	if len(p) >= size {
		copy(r.buf, p[len(p)-size:])
		r.start = 0
		r.n = size
		return len(p), nil
	}
	end := (r.start + r.n) % size
	first := copy(r.buf[end:], p)
	if first < len(p) {
		copy(r.buf, p[first:])
	}
	r.n += len(p)
	if r.n > size {
		r.start = (r.start + r.n - size) % size
		r.n = size
	}
	return len(p), nil
}

// Bytes returns a copy of the retained bytes, oldest first.
func (r *Ring) Bytes() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]byte, r.n)
	first := copy(out, r.buf[r.start:min(r.start+r.n, len(r.buf))])
	if first < r.n {
		copy(out[first:], r.buf[:r.n-first])
	}
	return out
}

// Len returns the number of retained bytes.
func (r *Ring) Len() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.n
}

// Reset drops everything.
func (r *Ring) Reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.start, r.n = 0, 0
}
