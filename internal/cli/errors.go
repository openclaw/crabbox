package cli

import "errors"

type ExitError struct {
	Code    int
	Message string
}

func (e ExitError) Error() string {
	return e.Message
}

func AsExitError(err error, target *ExitError) bool {
	return errors.As(err, target)
}

// ExitCodeForError returns the first matched ExitError's nonzero code, or fallback.
// Nil errors and matched zero codes use fallback; signed codes are preserved.
func ExitCodeForError(err error, fallback int) int {
	var exitErr ExitError
	if AsExitError(err, &exitErr) && exitErr.Code != 0 {
		return exitErr.Code
	}
	return fallback
}

func Exit(code int, format string, args ...any) ExitError {
	return ExitError{Code: code, Message: sprintf(format, args...)}
}
