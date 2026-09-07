package shared

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// EnvdProcessEnd is a process event, not the final Connect RPC envelope.
type EnvdProcessEnd struct {
	ExitCode int    `json:"exitCode"`
	Exited   bool   `json:"exited"`
	Status   string `json:"status"`
	Error    string `json:"error"`
}

type envdProcessResponse struct {
	Event struct {
		Start *struct {
			PID uint32 `json:"pid"`
		} `json:"start,omitempty"`
		Data *struct {
			Stdout string `json:"stdout"`
			Stderr string `json:"stderr"`
			PTY    string `json:"pty"`
		} `json:"data,omitempty"`
		End       *EnvdProcessEnd `json:"end,omitempty"`
		Keepalive map[string]any  `json:"keepalive,omitempty"`
	} `json:"event"`
}

func encodeConnectJSONEnvelope(v any) ([]byte, error) {
	data, err := json.Marshal(v)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	out.WriteByte(0)
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(data)))
	out.Write(size[:])
	out.Write(data)
	return out.Bytes(), nil
}

// ParseEnvdProcessStream owns wire decoding and delivery. An interpreter error
// stops at that process end with its code; otherwise later RPC/read errors win.
func ParseEnvdProcessStream(provider string, r io.Reader, stdout, stderr io.Writer, interpretEnd func(EnvdProcessEnd, io.Writer, ...string) (int, error), secrets ...string) (int, error) {
	exitCode := 0
	seenEnd := false
	for {
		var header [5]byte
		if _, err := io.ReadFull(r, header[:]); err != nil {
			if err == io.EOF {
				break
			}
			return 1, err
		}
		flags := header[0]
		size := binary.BigEndian.Uint32(header[1:])
		if flags&1 != 0 {
			return 1, fmt.Errorf("compressed connect envelopes are not supported")
		}
		data := make([]byte, size)
		if _, err := io.ReadFull(r, data); err != nil {
			return 1, err
		}
		if flags&2 != 0 {
			var end struct {
				Error *struct {
					Code    string `json:"code"`
					Message string `json:"message"`
				} `json:"error,omitempty"`
			}
			if len(data) > 0 {
				if err := json.Unmarshal(data, &end); err != nil {
					return 1, err
				}
			}
			if end.Error != nil {
				return 1, errors.New(RedactErrorSecrets(end.Error.Code+": "+end.Error.Message, secrets...))
			}
			break
		}
		var event envdProcessResponse
		if err := json.Unmarshal(data, &event); err != nil {
			return 1, err
		}
		if event.Event.Data != nil {
			if err := writeEnvdBase64(event.Event.Data.Stdout, stdout); err != nil {
				return 1, err
			}
			if err := writeEnvdBase64(event.Event.Data.Stderr, stderr); err != nil {
				return 1, err
			}
		}
		if event.Event.End != nil {
			code, err := interpretEnd(*event.Event.End, stderr, secrets...)
			exitCode = code
			seenEnd = true
			if err != nil {
				return code, err
			}
		}
	}
	if !seenEnd {
		return 1, fmt.Errorf("%s process stream ended without end event", provider)
	}
	return exitCode, nil
}

func writeEnvdBase64(value string, w io.Writer) error {
	if value == "" {
		return nil
	}
	data, err := base64.StdEncoding.DecodeString(value)
	if err != nil {
		return err
	}
	_, err = w.Write(data)
	return err
}
