package backend

// These buffers deliberately do not embed bytes.Buffer: os/exec copies a
// subprocess's output with io.Copy, which uses the destination's ReadFrom if
// it has one, bypassing any limit enforced in Write.

// headBuffer keeps the first limit bytes written to it and notes whether more arrived.
type headBuffer struct {
	buf       []byte
	limit     int
	truncated bool
}

func (b *headBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if room := b.limit - len(b.buf); len(p) > room {
		b.truncated = true
		p = p[:max(room, 0)]
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *headBuffer) Bytes() []byte  { return b.buf }
func (b *headBuffer) String() string { return string(b.buf) }

// tailBuffer keeps the last limit bytes written to it. A CLI's real error
// message comes at the end of its stderr, after any transcript it printed.
type tailBuffer struct {
	buf   []byte
	limit int
}

func (b *tailBuffer) Write(p []byte) (int, error) {
	n := len(p)
	if len(p) >= b.limit {
		b.buf = append(b.buf[:0], p[len(p)-b.limit:]...)
		return n, nil
	}
	if over := len(b.buf) + len(p) - b.limit; over > 0 {
		b.buf = append(b.buf[:0], b.buf[over:]...)
	}
	b.buf = append(b.buf, p...)
	return n, nil
}

func (b *tailBuffer) String() string { return string(b.buf) }
