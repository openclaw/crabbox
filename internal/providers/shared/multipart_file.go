package shared

import (
	"context"
	"errors"
	"io"
	"mime/multipart"
)

// WithMultipartFile owns one multipart "file" producer through exchange completion.
// It never closes source. Cancellation aborts pipe work, but returning may wait
// for an in-flight noncooperative source Read. Exchange retains HTTP/error policy;
// producerError must produce a safe nonnil error without discarding its cause.
func WithMultipartFile(ctx context.Context, filename string, source io.Reader, exchange func(io.ReadCloser, string) error, producerError func(error) error) (err error) {
	if producerError == nil {
		return errors.New("multipart upload requires a producer error policy")
	}
	pr, pw := io.Pipe()
	writer := multipart.NewWriter(pw)
	done := make(chan error, 1)
	stopCancel := context.AfterFunc(ctx, func() { _ = pr.CloseWithError(ctx.Err()) })
	defer stopCancel()
	defer pr.Close()
	go func() {
		part, produceErr := writer.CreateFormFile("file", filename)
		if produceErr == nil {
			_, produceErr = io.Copy(part, source)
		}
		if produceErr == nil {
			produceErr = writer.Close()
		}
		_ = pw.CloseWithError(produceErr)
		done <- produceErr
	}()
	returned := false
	defer func() {
		if !returned {
			_ = pr.CloseWithError(errors.New("multipart exchange did not return"))
		} else if err != nil {
			_ = pr.CloseWithError(err)
		}
		produceErr := <-done
		if returned && err == nil && produceErr != nil {
			err = producerError(produceErr)
			if err == nil {
				err = errors.New("multipart producer failed")
			}
		}
	}()
	err = exchange(pr, writer.FormDataContentType())
	returned = true
	return err
}
