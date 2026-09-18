//go:build !linux

package remoteruntime

import (
	"context"
	"errors"
	"io"
	"os"
)

func run(context.Context, request, *os.File, *os.File, *os.File) (int, error) {
	return failureCode, errors.New("native remote supervision requires Linux")
}

func runGuard() int { return failureCode }

func control(context.Context, string, string, io.Writer) error {
	return errors.New("native remote supervision requires Linux")
}
