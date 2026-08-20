package pgwire

import (
	"encoding/binary"
	"fmt"
	"io"
)

// Front-end message types, back-end message types and the two
// protocol constants, named so the protocol reads as protocol.
const (
	msgParse           = 'P'
	msgBind            = 'B'
	msgDescribe        = 'D'
	msgExecute         = 'E'
	msgSync            = 'S'
	msgQuery           = 'Q'
	msgPasswordMessage = 'p'
	msgTerminate       = 'X'

	msgAuthentication   = 'R'
	msgBackendKeyData   = 'K'
	msgBindComplete     = '2'
	msgCloseComplete    = '3'
	msgCommandComplete  = 'C'
	msgDataRow          = 'D'
	msgEmptyQuery       = 'I'
	msgErrorResponse    = 'E'
	msgNoData           = 'n'
	msgNoticeResponse   = 'N'
	msgNotification     = 'A'
	msgParameterDesc    = 't'
	msgParameterStatus  = 'S'
	msgParseComplete    = '1'
	msgPortalSuspended  = 's'
	msgReadyForQuery    = 'Z'
	msgRowDescription   = 'T'
	msgCopyInResponse   = 'G'
	msgCopyOutResponse  = 'H'
	msgCopyBothResponse = 'W'

	protocolVersion3 = 196608   // 3.0, as major<<16 | minor
	sslRequestCode   = 80877103 // the magic "version" that asks for TLS
)

// writeBuf accumulates one front-end message. start records where the
// length placeholder went so finish can patch it; the length counts
// itself but not the type byte, which is why startup messages (which
// have no type byte) use startUntyped.
type writeBuf struct {
	b   []byte
	pos int
}

func (w *writeBuf) start(typ byte) {
	w.b = append(w.b, typ)
	w.startUntyped()
}

func (w *writeBuf) startUntyped() {
	w.pos = len(w.b)
	w.b = append(w.b, 0, 0, 0, 0)
}

func (w *writeBuf) finish() {
	binary.BigEndian.PutUint32(w.b[w.pos:], uint32(len(w.b)-w.pos))
}

func (w *writeBuf) int32(v int32) {
	w.b = binary.BigEndian.AppendUint32(w.b, uint32(v))
}

func (w *writeBuf) int16(v int16) {
	w.b = binary.BigEndian.AppendUint16(w.b, uint16(v))
}

func (w *writeBuf) byte(v byte) { w.b = append(w.b, v) }

// str writes a NUL-terminated string.
func (w *writeBuf) str(s string) {
	w.b = append(w.b, s...)
	w.b = append(w.b, 0)
}

func (w *writeBuf) bytes(p []byte) { w.b = append(w.b, p...) }

// readBuf walks the body of one back-end message.
type readBuf struct {
	b   []byte
	err error
}

func (r *readBuf) int32() int32 {
	if len(r.b) < 4 {
		r.fail()
		return 0
	}
	v := int32(binary.BigEndian.Uint32(r.b))
	r.b = r.b[4:]
	return v
}

func (r *readBuf) int16() int16 {
	if len(r.b) < 2 {
		r.fail()
		return 0
	}
	v := int16(binary.BigEndian.Uint16(r.b))
	r.b = r.b[2:]
	return v
}

func (r *readBuf) byte() byte {
	if len(r.b) < 1 {
		r.fail()
		return 0
	}
	v := r.b[0]
	r.b = r.b[1:]
	return v
}

// str reads a NUL-terminated string.
func (r *readBuf) str() string {
	for i := 0; i < len(r.b); i++ {
		if r.b[i] == 0 {
			s := string(r.b[:i])
			r.b = r.b[i+1:]
			return s
		}
	}
	r.fail()
	return ""
}

// next returns the next n bytes as a fresh slice, so the caller may
// keep it after the read buffer is recycled.
func (r *readBuf) next(n int) []byte {
	if n < 0 || len(r.b) < n {
		r.fail()
		return nil
	}
	out := make([]byte, n)
	copy(out, r.b[:n])
	r.b = r.b[n:]
	return out
}

func (r *readBuf) rest() []byte {
	out := r.b
	r.b = nil
	return out
}

func (r *readBuf) fail() {
	if r.err == nil {
		r.err = fmt.Errorf("drops: truncated message from the server")
	}
	r.b = nil
}

// recv reads one back-end message. The returned body is valid until
// the next call: it aliases the connection's scratch buffer, so a
// caller that keeps a value out of it must copy first (see readBuf.next).
func (c *Conn) recv() (byte, *readBuf, error) {
	var head [5]byte
	if _, err := io.ReadFull(c.r, head[:]); err != nil {
		return 0, nil, c.fatal(fmt.Errorf("read message header: %w", err))
	}
	length := int(binary.BigEndian.Uint32(head[1:])) - 4
	if length < 0 || length > maxMessageSize {
		return 0, nil, c.fatal(fmt.Errorf("server announced a %d-byte message, which is out of range", length))
	}
	if cap(c.scratch) < length {
		c.scratch = make([]byte, length)
	}
	body := c.scratch[:length]
	if _, err := io.ReadFull(c.r, body); err != nil {
		return 0, nil, c.fatal(fmt.Errorf("read message body: %w", err))
	}
	return head[0], &readBuf{b: body}, nil
}

// maxMessageSize caps what the server may announce. A protocol
// desynchronisation shows up as a preposterous length, and refusing it
// is better than allocating whatever number was on the wire.
const maxMessageSize = 1 << 30

// send flushes one prepared message set to the server.
func (c *Conn) send(w *writeBuf) error {
	if _, err := c.w.Write(w.b); err != nil {
		return c.fatal(fmt.Errorf("write to server: %w", err))
	}
	if err := c.w.Flush(); err != nil {
		return c.fatal(fmt.Errorf("flush to server: %w", err))
	}
	return nil
}
