// Package runner serves bounded operations over an already authenticated byte
// stream. It has no credentials, provider routing, or listening network socket.
package runner

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"reflect"
	"runtime"
	"time"
	"unicode/utf8"

	"github.com/openclaw/crabbox/internal/runner/runnerfs"
	"github.com/openclaw/crabbox/internal/runner/runnerwire"
)

const (
	ExplicitMaxFiles      = 4096
	ExplicitMaxFileBytes  = 64 << 20
	ExplicitMaxTotalBytes = 256 << 20
)

func validateResultPaths(paths []string) error {
	if len(paths) > ExplicitMaxFiles {
		return fmt.Errorf("explicit result path limit is %d", ExplicitMaxFiles)
	}
	return nil
}

// BuildID is injected from the bundled source digest when building a helper.
var BuildID = "development"

// MaxRequestBytes bounds the encoded stream, including framing overhead.
func MaxRequestBytes() int64 {
	return runnerfs.DefaultArchiveLimits().MaxCompressedBytes*2 + runnerwire.MaxHeaderBytes*2
}

type Identity struct {
	BuildID  string `json:"buildId"`
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Protocol int    `json:"protocol"`
}

func CurrentIdentity() Identity {
	return Identity{BuildID: BuildID, OS: runtime.GOOS, Arch: runtime.GOARCH, Protocol: runnerwire.Version}
}

type Operation string

const (
	Collect  Operation = "collect"
	Upload   Operation = "upload"
	Download Operation = "download"
)

type Request struct {
	BuildID     string                 `json:"buildId"`
	Operation   Operation              `json:"operation"`
	Workdir     string                 `json:"workdir"`
	Paths       []string               `json:"paths"`
	Auto        bool                   `json:"auto"`
	Marker      string                 `json:"marker"`
	SourcePath  string                 `json:"sourcePath"`
	Destination string                 `json:"destination"`
	Source      runnerfs.ArchiveSource `json:"source"`
}

type FileInfo struct {
	Path    string                 `json:"path"`
	ModTime time.Time              `json:"modTime"`
	Archive bool                   `json:"archive"`
	Source  runnerfs.ArchiveSource `json:"source"`
}

type Outcome struct {
	Warnings  []runnerfs.Warning `json:"warnings"`
	Published bool               `json:"published"`
}

type RemoteError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e RemoteError) Error() string { return "remote runner: " + e.Message }

func writeFrame(output io.Writer, kind runnerwire.Kind, metadata any, size uint64, body io.Reader) error {
	var meta []byte
	var err error
	if metadata != nil {
		if err := validateMetadataText(reflect.ValueOf(metadata)); err != nil {
			return err
		}
		meta, err = json.Marshal(metadata)
		if err != nil {
			return err
		}
	}
	return runnerwire.WriteFrame(output, runnerwire.Header{Kind: kind, Meta: meta, Size: size}, body)
}

// JSON replaces invalid UTF-8 silently. A path must never name different bytes
// at the receiver, especially for a mutating upload.
func validateMetadataText(value reflect.Value) error {
	switch value.Kind() {
	case reflect.String:
		if !utf8.ValidString(value.String()) {
			return runnerfs.InvalidArchiveError{Message: "runner metadata contains invalid UTF-8"}
		}
	case reflect.Struct:
		for i := 0; i < value.NumField(); i++ {
			if value.Type().Field(i).IsExported() {
				if err := validateMetadataText(value.Field(i)); err != nil {
					return err
				}
			}
		}
	case reflect.Slice, reflect.Array:
		for i := 0; i < value.Len(); i++ {
			if err := validateMetadataText(value.Index(i)); err != nil {
				return err
			}
		}
	case reflect.Pointer, reflect.Interface:
		if !value.IsNil() {
			return validateMetadataText(value.Elem())
		}
	}
	return nil
}

func decodeObject(data []byte, value any) error {
	if err := exactMetadataKeys(data, reflect.TypeOf(value)); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	return decoder.Decode(value)
}

func exactMetadataKeys(data []byte, schema reflect.Type) error {
	for schema.Kind() == reflect.Pointer {
		schema = schema.Elem()
	}
	if reflect.PointerTo(schema).Implements(reflect.TypeFor[json.Unmarshaler]()) {
		return nil
	}
	if schema.Kind() == reflect.Slice {
		var values []json.RawMessage
		if err := json.Unmarshal(data, &values); err != nil {
			return err
		}
		for _, value := range values {
			if err := exactMetadataKeys(value, schema.Elem()); err != nil {
				return err
			}
		}
		return nil
	}
	if schema.Kind() != reflect.Struct {
		return nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("runner metadata must be an object")
	}
	known := make(map[string]reflect.Type, schema.NumField())
	for index := range schema.NumField() {
		field := schema.Field(index)
		known[field.Tag.Get("json")] = field.Type
	}
	for key, value := range fields {
		fieldType, ok := known[key]
		if !ok {
			return fmt.Errorf("unknown runner metadata field %q", key)
		}
		if err := exactMetadataKeys(value, fieldType); err != nil {
			return err
		}
	}
	return nil
}

func readEnd(input *runnerwire.Reader) error {
	frame, err := input.Next()
	if err != nil {
		return err
	}
	if frame.Header.Kind != runnerwire.End || len(frame.Header.Meta) != 0 {
		return errors.New("missing runner end frame")
	}
	if _, err := input.Next(); err != io.EOF {
		return fmt.Errorf("runner stream has trailing data: %v", err)
	}
	return nil
}
