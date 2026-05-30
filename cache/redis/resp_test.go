package redis

import (
	"bufio"
	"bytes"
	"errors"
	"strings"
	"testing"
)

// --- writeCommand --------------------------------------------------

func TestWriteCommandShapesArrayOfBulkStrings(t *testing.T) {
	var buf bytes.Buffer
	bw := bufio.NewWriter(&buf)
	if err := writeCommand(bw, "SET", "k", []byte("v"), "PX", 1500); err != nil {
		t.Fatal(err)
	}
	want := "*5\r\n$3\r\nSET\r\n$1\r\nk\r\n$1\r\nv\r\n$2\r\nPX\r\n$4\r\n1500\r\n"
	if got := buf.String(); got != want {
		t.Errorf("\n got: %q\nwant: %q", got, want)
	}
}

func TestWriteCommandHandlesEmptyValue(t *testing.T) {
	var buf bytes.Buffer
	if err := writeCommand(bufio.NewWriter(&buf), "SET", "k", ""); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "$0\r\n\r\n") {
		t.Errorf("empty bulk: %q", buf.String())
	}
}

// --- readReply -----------------------------------------------------

func TestReadReplyEveryType(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want reply
	}{
		{"simple string", "+OK\r\n", reply{kind: '+', str: "OK"}},
		{"error", "-ERR oops\r\n", reply{kind: '-', str: "ERR oops"}},
		{"integer", ":42\r\n", reply{kind: ':', int64: 42}},
		{"bulk", "$5\r\nhello\r\n", reply{kind: '$', bulk: []byte("hello")}},
		{"nil bulk", "$-1\r\n", reply{kind: '$', bulk: nil}},
		{"empty bulk", "$0\r\n\r\n", reply{kind: '$', bulk: []byte{}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := readReply(bufio.NewReader(strings.NewReader(tc.in)))
			if err != nil {
				t.Fatal(err)
			}
			if r.kind != tc.want.kind {
				t.Errorf("kind = %c, want %c", r.kind, tc.want.kind)
			}
			if r.str != tc.want.str {
				t.Errorf("str = %q, want %q", r.str, tc.want.str)
			}
			if r.int64 != tc.want.int64 {
				t.Errorf("int = %d, want %d", r.int64, tc.want.int64)
			}
			if string(r.bulk) != string(tc.want.bulk) {
				t.Errorf("bulk = %q, want %q", r.bulk, tc.want.bulk)
			}
			if tc.want.bulk == nil && tc.name == "nil bulk" && !r.isNil() {
				t.Error("nil bulk: isNil() = false")
			}
		})
	}
}

func TestReadReplyArrayRecurses(t *testing.T) {
	in := "*3\r\n$3\r\nfoo\r\n$-1\r\n:7\r\n"
	r, err := readReply(bufio.NewReader(strings.NewReader(in)))
	if err != nil {
		t.Fatal(err)
	}
	if r.kind != '*' || len(r.array) != 3 {
		t.Fatalf("array shape: %+v", r)
	}
	if string(r.array[0].bulk) != "foo" {
		t.Errorf("array[0] = %q", r.array[0].bulk)
	}
	if !r.array[1].isNil() {
		t.Errorf("array[1] should be nil")
	}
	if r.array[2].int64 != 7 {
		t.Errorf("array[2] = %d", r.array[2].int64)
	}
}

func TestReadReplyNilArray(t *testing.T) {
	r, err := readReply(bufio.NewReader(strings.NewReader("*-1\r\n")))
	if err != nil {
		t.Fatal(err)
	}
	if !r.isNil() {
		t.Errorf("array *-1 should report nil")
	}
}

func TestReadReplyRejectsMissingCRLF(t *testing.T) {
	_, err := readReply(bufio.NewReader(strings.NewReader("+OK\n")))
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("expected ErrProtocol, got %v", err)
	}
}

func TestReadReplyRejectsUnknownByte(t *testing.T) {
	_, err := readReply(bufio.NewReader(strings.NewReader("?bogus\r\n")))
	if !errors.Is(err, ErrProtocol) {
		t.Errorf("expected ErrProtocol, got %v", err)
	}
}
