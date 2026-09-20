package runner

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"strconv"
	"sync"
)

type cancellationInput struct {
	ctx   context.Context
	input io.Reader
}

func (r cancellationInput) Read(data []byte) (int, error) {
	if err := context.Cause(r.ctx); err != nil {
		return 0, err
	}
	n, err := r.input.Read(data)
	if cause := context.Cause(r.ctx); cause != nil {
		return n, errors.Join(err, cause)
	}
	return n, err
}

// Main keeps the installed executable and development bootstrap entrypoints on
// the same implementation. Base64 is only a transport for APIs with UTF-8 logs.
// Main owns input and output and closes both on cancellation and return.
// Their Close methods must interrupt pending I/O, as OS pipes do.
func Main(ctx context.Context, args []string, ownedInput io.ReadCloser, ownedOutput io.WriteCloser, diagnostic io.Writer) int {
	var once sync.Once
	closeStreams := func() {
		once.Do(func() {
			_ = ownedInput.Close()
			_ = ownedOutput.Close()
		})
	}
	closed := make(chan struct{})
	stop := context.AfterFunc(ctx, func() {
		closeStreams()
		close(closed)
	})
	defer func() {
		if !stop() {
			<-closed
		}
		closeStreams()
	}()
	var input io.Reader = cancellationInput{ctx: ctx, input: ownedInput}
	var output io.Writer = ownedOutput
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
