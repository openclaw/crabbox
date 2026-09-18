package runnerfs

import (
	"context"
	"io"
)

func Copy(ctx context.Context, dst io.Writer, src io.Reader) error {
	buffer := make([]byte, 128*1024)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		n, readErr := src.Read(buffer)
		if n > 0 {
			if err := ctx.Err(); err != nil {
				return err
			}
			written, err := dst.Write(buffer[:n])
			if err != nil {
				return err
			}
			if written != n {
				return io.ErrShortWrite
			}
		}
		if readErr == io.EOF {
			return ctx.Err()
		}
		if readErr != nil {
			return readErr
		}
	}
}
