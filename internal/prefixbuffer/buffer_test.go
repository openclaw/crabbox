package prefixbuffer

import (
	"io"
	"reflect"
	"strings"
	"testing"
)

func TestBufferRetention(t *testing.T) {
	for _, tc := range []struct {
		name   string
		buffer Buffer
		want   string
	}{
		{name: "zero value"},
		{name: "zero limit", buffer: NewLimited(0)},
		{name: "negative limit", buffer: NewLimited(-1)},
		{name: "finite", buffer: NewLimited(4), want: "abcd"},
		{name: "unlimited", buffer: NewUnlimited(), want: "abcdef"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			b := &tc.buffer
			input := ""
			for _, chunk := range []string{"", "abc", "d", "e", "", "f"} {
				n, err := b.Write([]byte(chunk))
				if n != len(chunk) || err != nil {
					t.Fatalf("write=%d/%v, want %d/nil", n, err, len(chunk))
				}
				input += chunk
				want := tc.want[:min(len(input), len(tc.want))]
				if b.String() != want || string(b.Bytes()) != want || b.Exceeded() != (len(input) > len(tc.want)) {
					t.Fatalf("input=%q retained=%q exceeded=%v, want %q/%v", input, b.String(), b.Exceeded(), want, len(input) > len(tc.want))
				}
			}
		})
	}
}

func TestBufferCopyAndMethodSurface(t *testing.T) {
	b := NewLimited(4)
	// Hide WriterTo so io.Copy uses the destination's ordinary writer contract.
	if n, err := io.Copy(&b, struct{ io.Reader }{strings.NewReader("abcde")}); err != nil || n != 5 {
		t.Fatalf("copy=%d/%v", n, err)
	}
	if n, err := io.WriteString(&b, "f"); err != nil || n != 1 {
		t.Fatalf("write string=%d/%v", n, err)
	}
	if b.String() != "abcd" || !b.Exceeded() {
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
