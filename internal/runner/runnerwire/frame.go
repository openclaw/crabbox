// Package runnerwire frames runner metadata and raw file bytes over an existing
// authenticated stream. It does not open sockets or confer filesystem authority.
package runnerwire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"unicode/utf8"
)

const Version = 1
const MaxHeaderBytes = 64 << 10

var magic = [4]byte{'C', 'B', 'X', 'R'}

var ErrUnreadPayload = errors.New("previous frame payload has not been consumed")

type Kind string

const (
	Hello   Kind = "hello"
	Request Kind = "request"
	File    Kind = "file"
	Result  Kind = "result"
	Error   Kind = "error"
	End     Kind = "end"
)

type Header struct {
	Version int             `json:"version"`
	Kind    Kind            `json:"kind"`
	Size    uint64          `json:"size"`
	Meta    json.RawMessage `json:"meta,omitempty"`
}

type Frame struct {
	Header Header
	Body   io.Reader
}

type Reader struct {
	reader  io.Reader
	maxBody uint64
	current *payload
	err     error
}

func NewReader(reader io.Reader, maxBody uint64) *Reader {
	return &Reader{reader: reader, maxBody: maxBody}
}

func (r *Reader) Next() (frame Frame, err error) {
	if r.err != nil {
		return frame, r.err
	}
	if r.current != nil && r.current.remaining != 0 {
		return frame, ErrUnreadPayload
	}
	defer func() {
		if err != nil {
			r.err = err
		}
	}()
	var prefix [8]byte
	if _, err = io.ReadFull(r.reader, prefix[:]); err != nil {
		return frame, err
	}
	if !bytes.Equal(prefix[:4], magic[:]) {
		return frame, errors.New("invalid runner frame magic")
	}
	size := binary.BigEndian.Uint32(prefix[4:])
	if size == 0 || size > MaxHeaderBytes {
		return frame, errors.New("runner frame header exceeds limit")
	}
	data := make([]byte, int(size))
	if _, err = io.ReadFull(r.reader, data); err != nil {
		return frame, err
	}
	if err = decodeHeader(data, &frame.Header); err != nil {
		return frame, err
	}
	if err = validateHeader(frame.Header, r.maxBody); err != nil {
		return frame, err
	}
	r.current = &payload{parent: r, remaining: frame.Header.Size}
	frame.Body = r.current
	return frame, nil
}

type payload struct {
	parent    *Reader
	remaining uint64
}

func (p *payload) Read(data []byte) (int, error) {
	if p.parent.err != nil {
		return 0, p.parent.err
	}
	if p.remaining == 0 {
		return 0, io.EOF
	}
	if uint64(len(data)) > p.remaining {
		data = data[:int(p.remaining)]
	}
	n, err := p.parent.reader.Read(data)
	if n < 0 || n > len(data) {
		err = errors.New("invalid payload reader count")
		n = 0
	}
	p.remaining -= uint64(n)
	if err == io.EOF && p.remaining != 0 {
		err = io.ErrUnexpectedEOF
	}
	if err != nil && err != io.EOF {
		p.parent.err = err
	}
	return n, err
}

// WriteFrame streams exactly Size bytes. A short source leaves an incomplete
// frame and returns an error; receivers must not publish an incomplete payload.
func WriteFrame(writer io.Writer, header Header, body io.Reader) error {
	if header.Version == 0 {
		header.Version = Version
	}
	if err := validateHeader(header, math.MaxInt64); err != nil {
		return err
	}
	if header.Size != 0 && body == nil {
		return errors.New("runner frame body is missing")
	}
	data, err := json.Marshal(header)
	if err != nil {
		return err
	}
	if len(data) > MaxHeaderBytes {
		return errors.New("runner frame header exceeds limit")
	}
	if err := uniqueObjectKeys(data); err != nil {
		return err
	}
	var prefix [8]byte
	copy(prefix[:4], magic[:])
	binary.BigEndian.PutUint32(prefix[4:], uint32(len(data)))
	for _, part := range [][]byte{prefix[:], data} {
		n, err := writer.Write(part)
		if err != nil {
			return err
		}
		if n != len(part) {
			return io.ErrShortWrite
		}
	}
	if header.Size == 0 {
		return nil
	}
	_, err = io.CopyN(writer, body, int64(header.Size))
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

func validateHeader(header Header, limit uint64) error {
	if header.Version != Version {
		return fmt.Errorf("unsupported runner protocol version %d", header.Version)
	}
	switch header.Kind {
	case Hello, Request, File, Result, Error, End:
	default:
		return errors.New("unknown runner frame kind")
	}
	if header.Size > limit || header.Size > math.MaxInt64 {
		return errors.New("runner frame payload exceeds limit")
	}
	if header.Size != 0 && header.Kind != Request && header.Kind != File {
		return errors.New("runner metadata frame cannot contain a payload")
	}
	if len(header.Meta) != 0 {
		meta := bytes.TrimSpace(header.Meta)
		if len(meta) == 0 || meta[0] != '{' {
			return errors.New("runner metadata must be an object")
		}
	}
	return nil
}

func decodeHeader(data []byte, header *Header) error {
	if err := uniqueObjectKeys(data); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	if fields == nil {
		return errors.New("runner header must be an object")
	}
	for key := range fields {
		switch key {
		case "version", "kind", "size", "meta":
		default:
			return fmt.Errorf("unknown runner header field %q", key)
		}
	}
	if value, ok := fields["size"]; !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
		return errors.New("runner header size is missing")
	}
	return json.Unmarshal(data, header)
}

func uniqueObjectKeys(data []byte) error {
	if !utf8.Valid(data) {
		return errors.New("runner metadata contains invalid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	var visit func(int) error
	visit = func(depth int) error {
		if depth > 64 {
			return errors.New("runner metadata nesting exceeds limit")
		}
		token, err := decoder.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		switch delim {
		case '{':
			keys := make(map[string]bool)
			for decoder.More() {
				key, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok {
					return errors.New("invalid runner metadata key")
				}
				if keys[name] {
					return fmt.Errorf("duplicate runner metadata key %q", name)
				}
				keys[name] = true
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for decoder.More() {
				if err := visit(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid runner metadata delimiter")
		}
		_, err = decoder.Token()
		return err
	}
	if err := visit(0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("trailing runner metadata")
	}
	return nil
}
