package redis

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"strconv"
)

// RESP2 protocol implementation.
//
// Wire-format reference (Redis docs):
//   +OK\r\n          -- simple string
//   -ERR msg\r\n     -- error
//   :123\r\n         -- integer
//   $5\r\nhello\r\n  -- bulk string ($-1 = nil)
//   *3\r\n...        -- array of N elements (*-1 = nil)
//
// Requests always go out as arrays of bulk strings (the "unified
// request protocol"); responses are any of the five forms above and
// recurse for arrays.

// writeCommand serialises a command + args as a RESP2 array of bulk
// strings and writes it to w. Args are stringified via fmt.Sprint —
// callers that need byte-level control can pass []byte directly.
func writeCommand(w *bufio.Writer, args ...any) error {
	if _, err := fmt.Fprintf(w, "*%d\r\n", len(args)); err != nil {
		return err
	}
	for _, a := range args {
		var b []byte
		switch v := a.(type) {
		case string:
			b = []byte(v)
		case []byte:
			b = v
		case int:
			b = []byte(strconv.Itoa(v))
		case int64:
			b = []byte(strconv.FormatInt(v, 10))
		default:
			b = []byte(fmt.Sprint(v))
		}
		if _, err := fmt.Fprintf(w, "$%d\r\n", len(b)); err != nil {
			return err
		}
		if _, err := w.Write(b); err != nil {
			return err
		}
		if _, err := w.Write(crlf); err != nil {
			return err
		}
	}
	return w.Flush()
}

var crlf = []byte{'\r', '\n'}

// reply is a tagged union of the five RESP2 reply types.
type reply struct {
	kind  byte    // '+', '-', ':', '$', '*'
	str   string  // simple string / error message
	int64 int64   // integer
	bulk  []byte  // bulk string contents (nil for $-1)
	array []reply // array elements (nil for *-1)
}

// isNil reports whether the reply is RESP's nil ($-1 or *-1).
func (r reply) isNil() bool {
	return (r.kind == '$' && r.bulk == nil) || (r.kind == '*' && r.array == nil)
}

// readReply parses a single reply from r. EOF / network errors come
// back as-is; protocol violations come back as ErrProtocol-wrapped.
func readReply(r *bufio.Reader) (reply, error) {
	b, err := r.ReadByte()
	if err != nil {
		return reply{}, err
	}
	line, err := readLine(r)
	if err != nil {
		return reply{}, err
	}
	switch b {
	case '+':
		return reply{kind: '+', str: string(line)}, nil
	case '-':
		return reply{kind: '-', str: string(line)}, nil
	case ':':
		n, err := strconv.ParseInt(string(line), 10, 64)
		if err != nil {
			return reply{}, fmt.Errorf("%w: int: %v", ErrProtocol, err)
		}
		return reply{kind: ':', int64: n}, nil
	case '$':
		n, err := strconv.Atoi(string(line))
		if err != nil {
			return reply{}, fmt.Errorf("%w: bulk len: %v", ErrProtocol, err)
		}
		if n < 0 {
			return reply{kind: '$', bulk: nil}, nil // nil bulk string
		}
		buf := make([]byte, n+2) // +2 for trailing CRLF
		if _, err := io.ReadFull(r, buf); err != nil {
			return reply{}, err
		}
		return reply{kind: '$', bulk: buf[:n]}, nil
	case '*':
		n, err := strconv.Atoi(string(line))
		if err != nil {
			return reply{}, fmt.Errorf("%w: array len: %v", ErrProtocol, err)
		}
		if n < 0 {
			return reply{kind: '*', array: nil}, nil
		}
		arr := make([]reply, n)
		for i := 0; i < n; i++ {
			arr[i], err = readReply(r)
			if err != nil {
				return reply{}, err
			}
		}
		return reply{kind: '*', array: arr}, nil
	default:
		return reply{}, fmt.Errorf("%w: unknown reply byte 0x%02x", ErrProtocol, b)
	}
}

// readLine reads until \r\n and returns the bytes before it.
func readLine(r *bufio.Reader) ([]byte, error) {
	b, err := r.ReadBytes('\n')
	if err != nil {
		return nil, err
	}
	if len(b) < 2 || b[len(b)-2] != '\r' {
		return nil, fmt.Errorf("%w: missing CRLF", ErrProtocol)
	}
	return b[:len(b)-2], nil
}

// ErrProtocol is wrapped by every malformed-reply error. errors.Is
// against it succeeds.
var ErrProtocol = errors.New("redis: protocol error")
