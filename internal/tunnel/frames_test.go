package tunnel

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func TestEncodeDecodeFrameRoundTrip(t *testing.T) {
	tests := []struct {
		name  string
		frame any
	}{
		{
			name:  FrameResHead,
			frame: &ResHead{ID: "1", StatusCode: 200, Headers: map[string]string{"Content-Type": "text/event-stream"}},
		},
		{
			name:  FrameResChunk,
			frame: &ResChunk{ID: "1", Seq: 3, Data: []byte("hello")},
		},
		{
			name:  FrameResEnd,
			frame: &ResEnd{ID: "1"},
		},
		{
			name:  FrameResError,
			frame: &ResError{ID: "1", Message: "boom"},
		},
		{
			name:  FrameReqCancel,
			frame: &ReqCancel{ID: "1"},
		},
		{
			name:  FrameWsOpen,
			frame: &WsOpen{ID: "2", Path: "/pty", Headers: map[string]string{"X-Test": "yes"}, Subprotocols: []string{"tty"}},
		},
		{
			name:  FrameWsAck,
			frame: &WsAck{ID: "2", OK: true, Status: 101, Subprotocol: "tty"},
		},
		{
			name:  FrameWsFrame,
			frame: &WsFrame{ID: "2", Opcode: 1, Data: []byte{0x00, 0x01, 0xff}},
		},
		{
			name:  FrameWsClose,
			frame: &WsClose{ID: "2", Code: 1000, Reason: "bye"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			data, err := encodeFrame(tt.frame)
			if err != nil {
				t.Fatalf("encodeFrame() error = %v", err)
			}

			var probe struct {
				Type string `json:"type"`
			}
			if err := json.Unmarshal(data, &probe); err != nil {
				t.Fatalf("encoded frame is not valid JSON: %v (%s)", err, data)
			}
			if probe.Type != tt.name {
				t.Fatalf("type = %q, want %q", probe.Type, tt.name)
			}

			got, err := decodeFrame(data)
			if err != nil {
				t.Fatalf("decodeFrame() error = %v", err)
			}
			if !reflect.DeepEqual(got, tt.frame) {
				t.Fatalf("round-trip = %#v, want %#v", got, tt.frame)
			}
		})
	}
}

func TestEncodeDecodeLegacyTunnelResponse(t *testing.T) {
	want := &TunnelResponse{
		ID:         "legacy-1",
		StatusCode: 201,
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       []byte(`{"ok":true}`),
	}

	data, err := encodeFrame(want)
	if err != nil {
		t.Fatalf("encodeFrame() error = %v", err)
	}
	if bytes.Contains(data, []byte(`"type"`)) {
		t.Fatalf("legacy response must not carry a type field: %s", data)
	}

	got, err := decodeFrame(data)
	if err != nil {
		t.Fatalf("decodeFrame() error = %v", err)
	}
	resp, ok := got.(*TunnelResponse)
	if !ok {
		t.Fatalf("decodeFrame() = %T, want *TunnelResponse", got)
	}
	if !reflect.DeepEqual(resp, want) {
		t.Fatalf("round-trip = %#v, want %#v", resp, want)
	}
}

func TestDecodeFrameUnknownType(t *testing.T) {
	_, err := decodeFrame([]byte(`{"type":"bogus","id":"1"}`))
	if !errors.Is(err, ErrUnknownFrame) {
		t.Fatalf("decodeFrame() error = %v, want ErrUnknownFrame", err)
	}
}

func TestEncodeFrameTooLarge(t *testing.T) {
	frame := &ResChunk{ID: "1", Seq: 0, Data: bytes.Repeat([]byte("a"), maxFrameBytes+1)}
	if _, err := encodeFrame(frame); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("encodeFrame() error = %v, want ErrFrameTooLarge", err)
	}
}

func TestDecodeFrameTooLarge(t *testing.T) {
	payload := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte("a"), maxFrameBytes+1))
	raw := `{"type":"res-chunk","id":"1","seq":0,"data":"` + payload + `"}`
	if _, err := decodeFrame([]byte(raw)); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("decodeFrame() error = %v, want ErrFrameTooLarge", err)
	}
}

func TestFrameBase64Integrity(t *testing.T) {
	data := make([]byte, 256)
	for i := range data {
		data[i] = byte(i)
	}
	frame := &WsFrame{ID: "ws", Opcode: 2, Data: data}

	encoded, err := encodeFrame(frame)
	if err != nil {
		t.Fatalf("encodeFrame() error = %v", err)
	}
	if !strings.Contains(string(encoded), base64.StdEncoding.EncodeToString(data)) {
		t.Fatalf("encoded frame does not contain base64 payload: %s", encoded)
	}

	decoded, err := decodeFrame(encoded)
	if err != nil {
		t.Fatalf("decodeFrame() error = %v", err)
	}
	got, ok := decoded.(*WsFrame)
	if !ok {
		t.Fatalf("decodeFrame() = %T, want *WsFrame", decoded)
	}
	if !bytes.Equal(got.Data, data) {
		t.Fatalf("data = %v, want %v", got.Data, data)
	}
}

func TestDecodeFrameMalformed(t *testing.T) {
	if _, err := decodeFrame([]byte(`{`)); err == nil {
		t.Fatal("decodeFrame() error = nil, want error")
	}
}
