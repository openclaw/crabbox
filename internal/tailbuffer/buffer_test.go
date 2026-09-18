package tailbuffer

import (
	"bytes"
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestBufferRetention(t *testing.T) {
	for _, tc := range []struct {
		name   string
		buffer Buffer
		limit  int
		chunks [][]byte
	}{
		{name: "zero value", chunks: [][]byte{nil, []byte("a"), nil}},
		{name: "zero limit", buffer: NewLimited(0), chunks: [][]byte{nil, []byte("ab"), nil}},
		{name: "negative limit", buffer: NewLimited(-1), chunks: [][]byte{[]byte("ab"), nil}},
		{name: "split crossing", buffer: NewLimited(4), limit: 4, chunks: [][]byte{nil, []byte("ab"), []byte("cd"), nil, []byte("e"), []byte("fg"), nil}},
		{name: "exact single write", buffer: NewLimited(4), limit: 4, chunks: [][]byte{[]byte("abcd"), nil}},
		{name: "exact replaces previous", buffer: NewLimited(4), limit: 4, chunks: [][]byte{[]byte("a"), []byte("bcde"), nil}},
		{name: "larger chunk", buffer: NewLimited(3), limit: 3, chunks: [][]byte{[]byte("ab"), []byte("cdefghi"), []byte("j")}},
		{name: "raw bytes", buffer: NewLimited(3), limit: 3, chunks: [][]byte{{'a', 0xf0, 0x9f}, {0x98, 0x80}, {0xff, 0}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var input []byte
			for _, chunk := range tc.chunks {
				n, err := tc.buffer.Write(chunk)
				if n != len(chunk) || err != nil {
					t.Fatalf("write=%d/%v, want %d/nil", n, err, len(chunk))
				}
				input = append(input, chunk...)
				want := input[max(0, len(input)-tc.limit):]
				if !bytes.Equal(tc.buffer.Bytes(), want) || tc.buffer.String() != string(want) || tc.buffer.Exceeded() != (len(input) > tc.limit) {
					t.Fatalf("input=%q retained=%q exceeded=%v, want %q/%v", input, tc.buffer.Bytes(), tc.buffer.Exceeded(), want, len(input) > tc.limit)
				}
			}
		})
	}
}

func TestBufferCopyAndMethodSurface(t *testing.T) {
	b := NewLimited(4)
	if n, err := io.Copy(&b, struct{ io.Reader }{strings.NewReader("abcde")}); err != nil || n != 5 {
		t.Fatalf("copy=%d/%v", n, err)
	}
	if n, err := io.WriteString(&b, "f"); err != nil || n != 1 {
		t.Fatalf("write string=%d/%v", n, err)
	}
	if b.String() != "cdef" || !b.Exceeded() {
		t.Fatalf("retained=%q exceeded=%v", b.String(), b.Exceeded())
	}
	var methods []string
	bufferType := reflect.TypeOf(&b)
	for i := 0; i < bufferType.NumMethod(); i++ {
		methods = append(methods, bufferType.Method(i).Name)
	}
	if want := []string{"Bytes", "Exceeded", "String", "Write"}; !reflect.DeepEqual(methods, want) {
		t.Fatalf("method surface=%v, want %v", methods, want)
	}
}
