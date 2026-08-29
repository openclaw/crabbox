package cli

import (
	"errors"
	"io"
	"strings"
)

const credentialInputMaxBytes = 4 << 10

var (
	errCredentialInputTooLarge = errors.New("credential input too large")
	errCredentialInputEmpty    = errors.New("credential input empty")
)

// readCredentialInput preserves exact bytes; the receiving owner validates
// their format, reports errors, and decides when to close its input.
func readCredentialInput(r io.Reader) (string, error) {
	data, err := io.ReadAll(io.LimitReader(r, credentialInputMaxBytes+1))
	if err != nil {
		return "", err
	}
	if len(data) > credentialInputMaxBytes {
		return "", errCredentialInputTooLarge
	}
	value := string(data)
	if strings.TrimSpace(value) == "" {
		return "", errCredentialInputEmpty
	}
	return value, nil
}
