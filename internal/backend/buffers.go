package backend

import "sync"

// These buffers deliberately do not embed bytes.Buffer: os/exec and io.Copy
// use a destination's ReadFrom if it has one, which would bypass any limit
// enforced in Write. They lock so they can be read while a stray copy is
// still writing.

// headBuffer keeps the first limit bytes written to it and notes whether more arrived.
type headBuffer struct {
	mu        sync.Mutex
	buf       []byte
	limit     int
	truncated bool
}

func (b *headBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if room := b.limit - len(b.buf); len(p) > room {
		b.truncated = true
		p = p[:max(room, 0)]
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *headBuffer) Bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]byte(nil), b.buf...)
}

func (b *headBuffer) String() string { return string(b.Bytes()) }

func (b *headBuffer) Truncated() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.truncated
}

// tailBuffer keeps the last limit bytes written to it. A CLI's real error
// message comes at the end of its stderr, after any transcript it printed.
// It grows to twice the limit before trimming, so many small writes cost
// amortised constant time rather than a full shift each.
type tailBuffer struct {
	mu    sync.Mutex
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	n := len(p)
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return n, nil
	}
	b.buf = append(b.buf, p...)
	if len(b.buf) > 2*b.limit {
		b.buf = append(b.buf[:0], b.buf[len(b.buf)-b.limit:]...)
	}
	return n, nil
}

func (b *tailBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.buf) > b.limit {
		return string(b.buf[len(b.buf)-b.limit:])
	}
	return string(b.buf)
}
