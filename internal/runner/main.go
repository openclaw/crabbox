package runner

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"strconv"
)

// Main keeps the installed executable and development bootstrap entrypoints on
// the same implementation. Base64 is only a transport for APIs with UTF-8 logs.
func Main(ctx context.Context, args []string, input io.Reader, output, diagnostic io.Writer) int {
	if (len(args) != 1 && len(args) != 3) || (args[0] != "serve" && args[0] != "serve-base64") {
		fmt.Fprintln(diagnostic, "usage: crabbox-runner serve|serve-base64 [--input-bytes N]")
		return 2
	}
	if len(args) == 3 {
		size, err := strconv.ParseInt(args[2], 10, 64)
		if args[1] != "--input-bytes" || err != nil || size < 1 || size > MaxRequestBytes() {
			fmt.Fprintln(diagnostic, "invalid runner input byte count")
			return 2
		}
		// Some authenticated exec transports keep stdin open after its payload.
		// Their caller supplies the exact spooled byte count, before base64 decode.
		input = exactInput{&io.LimitedReader{R: input, N: size}}
	}
	var encoded io.WriteCloser
	if args[0] == "serve-base64" {
		input = base64.NewDecoder(base64.StdEncoding, input)
		encoded = base64.NewEncoder(base64.StdEncoding, output)
		output = encoded
	}
	err := Serve(ctx, input, output, CurrentIdentity())
	if encoded != nil {
		if closeErr := encoded.Close(); err == nil {
			err = closeErr
		}
	}
	if err != nil {
		return 1
	}
	return 0
}

type exactInput struct{ *io.LimitedReader }

func (r exactInput) Read(data []byte) (int, error) {
	n, err := r.LimitedReader.Read(data)
	if err == io.EOF && r.N != 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
