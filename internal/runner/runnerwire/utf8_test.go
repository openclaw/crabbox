package runnerwire

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestRejectInvalidUTF8Metadata(t *testing.T) {
	meta := json.RawMessage("{\"path\":\"name-\xff\"}")
	var encoded bytes.Buffer
	if err := WriteFrame(&encoded, Header{Kind: Request, Meta: meta}, nil); err == nil || encoded.Len() != 0 {
		t.Fatalf("invalid metadata was written: %d bytes, %v", encoded.Len(), err)
	}
	data := []byte("{\"version\":1,\"kind\":\"request\",\"size\":0,\"meta\":" + string(meta) + "}")
	prefix := []byte{'C', 'B', 'X', 'R', 0, 0, 0, 0}
	binary.BigEndian.PutUint32(prefix[4:], uint32(len(data)))
	if _, err := NewReader(bytes.NewReader(append(prefix, data...)), 1024).Next(); err == nil {
		t.Fatal("invalid metadata was decoded lossily")
	}
}
