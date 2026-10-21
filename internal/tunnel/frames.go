package tunnel

import (
	"encoding/json"
	"errors"
)

// Protocol limits shared by the relay and the tunnel client.
const (
	// maxFrameBytes caps the payload of a single frame before base64
	// encoding. See design D6.
	maxFrameBytes = 1 << 20
	// MaxFrameBytes is the exported form used by the tunnel client.
	MaxFrameBytes = maxFrameBytes
)

// Frame type names. Legacy TunnelResponse frames have no type field.
const (
	FrameResHead   = "res-head"
	FrameResChunk  = "res-chunk"
	FrameResEnd    = "res-end"
	FrameResError  = "res-error"
	FrameReqCancel = "req-cancel"

	FrameWsOpen  = "ws-open"
	FrameWsAck   = "ws-ack"
	FrameWsFrame = "ws-frame"
	FrameWsClose = "ws-close"
)

var (
	// ErrUnknownFrame is returned by decodeFrame for frame types this relay
	// does not know. Callers should ignore such frames.
	ErrUnknownFrame = errors.New("unknown tunnel frame type")
	// ErrFrameTooLarge is returned when a frame exceeds maxFrameBytes.
	ErrFrameTooLarge = errors.New("tunnel frame too large")
)

// ResHead starts a streaming HTTP response.
type ResHead struct {
	ID         string            `json:"id"`
	StatusCode int               `json:"status_code"`
	Headers    map[string]string `json:"headers"`
}

// ResChunk carries one piece of a streaming response body.
type ResChunk struct {
	ID   string `json:"id"`
	Seq  int64  `json:"seq"`
	Data []byte `json:"data"`
}

// ResEnd terminates a streaming response successfully.
type ResEnd struct {
	ID string `json:"id"`
}

// ResError terminates a streaming response with an error.
type ResError struct {
	ID      string `json:"id"`
	Message string `json:"message"`
}

// ReqCancel asks the host to stop working on a stream.
type ReqCancel struct {
	ID string `json:"id"`
}

// WsOpen asks the host to dial a local WebSocket.
type WsOpen struct {
	ID           string            `json:"id"`
	Path         string            `json:"path"`
	Headers      map[string]string `json:"headers"`
	Subprotocols []string          `json:"subprotocols"`
}

// WsAck reports the result of a WsOpen. Encrypted tells the mobile client
// whether ws-frame payloads carry per-frame AES-256-GCM ciphertext.
type WsAck struct {
	ID          string `json:"id"`
	OK          bool   `json:"ok"`
	Status      int    `json:"status"`
	Error       string `json:"error"`
	Subprotocol string `json:"subprotocol"`
	Encrypted   bool   `json:"encrypted,omitempty"`
}

// WsFrame carries one WebSocket data frame.
type WsFrame struct {
	ID     string `json:"id"`
	Opcode int    `json:"opcode"`
	Data   []byte `json:"data"`
}

// WsClose carries a WebSocket close frame.
type WsClose struct {
	ID     string `json:"id"`
	Code   int    `json:"code"`
	Reason string `json:"reason"`
}

// EncodeFrame marshals a frame and adds its type field. Legacy
// *TunnelResponse frames are encoded without a type field.
func EncodeFrame(frame any) ([]byte, error) {
	return encodeFrame(frame)
}

// DecodeFrame parses a tunnel frame. Frames without a type field are decoded
// as legacy *TunnelResponse values.
func DecodeFrame(msg []byte) (any, error) {
	return decodeFrame(msg)
}

func frameType(frame any) (string, bool) {
	switch frame.(type) {
	case *ResHead:
		return FrameResHead, true
	case *ResChunk:
		return FrameResChunk, true
	case *ResEnd:
		return FrameResEnd, true
	case *ResError:
		return FrameResError, true
	case *ReqCancel:
		return FrameReqCancel, true
	case *WsOpen:
		return FrameWsOpen, true
	case *WsAck:
		return FrameWsAck, true
	case *WsFrame:
		return FrameWsFrame, true
	case *WsClose:
		return FrameWsClose, true
	case *TunnelResponse:
		return "", true
	default:
		return "", false
	}
}

func framePayloadSize(frame any) int {
	switch f := frame.(type) {
	case *ResChunk:
		return len(f.Data)
	case *WsFrame:
		return len(f.Data)
	case *TunnelResponse:
		return len(f.Body)
	default:
		return 0
	}
}

func encodeFrame(frame any) ([]byte, error) {
	if frame == nil {
		return nil, ErrUnknownFrame
	}
	if framePayloadSize(frame) > maxFrameBytes {
		return nil, ErrFrameTooLarge
	}

	typ, ok := frameType(frame)
	if !ok {
		return nil, ErrUnknownFrame
	}

	data, err := json.Marshal(frame)
	if err != nil {
		return nil, err
	}
	if typ == "" {
		return data, nil
	}

	// Inject the type discriminator without duplicating it in every struct.
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(data, &obj); err != nil {
		return nil, err
	}
	typeJSON, err := json.Marshal(typ)
	if err != nil {
		return nil, err
	}
	obj["type"] = typeJSON

	out, err := json.Marshal(obj)
	if err != nil {
		return nil, err
	}
	if len(out) > maxFrameBytes*2 {
		return nil, ErrFrameTooLarge
	}
	return out, nil
}

func decodeFrame(msg []byte) (any, error) {
	if len(msg) == 0 {
		return nil, ErrUnknownFrame
	}
	// Base64 expansion means a payload of maxFrameBytes can produce a JSON
	// message of roughly 4/3 its size. Anything beyond 2x is definitely over.
	if len(msg) > maxFrameBytes*2 {
		return nil, ErrFrameTooLarge
	}

	var probe struct {
		Type string `json:"type"`
	}
	if err := json.Unmarshal(msg, &probe); err != nil {
		return nil, err
	}

	switch probe.Type {
	case "":
		var resp TunnelResponse
		if err := json.Unmarshal(msg, &resp); err != nil {
			return nil, err
		}
		if len(resp.Body) > maxFrameBytes {
			return nil, ErrFrameTooLarge
		}
		return &resp, nil
	case FrameResHead:
		var f ResHead
		if err := json.Unmarshal(msg, &f); err != nil {
			return nil, err
		}
		return &f, nil
	case FrameResChunk:
		var f ResChunk
		if err := json.Unmarshal(msg, &f); err != nil {
			return nil, err
		}
		if len(f.Data) > maxFrameBytes {
			return nil, ErrFrameTooLarge
		}
		return &f, nil
	case FrameResEnd:
		var f ResEnd
		if err := json.Unmarshal(msg, &f); err != nil {
			return nil, err
		}
		return &f, nil
	case FrameResError:
		var f ResError
		if err := json.Unmarshal(msg, &f); err != nil {
			return nil, err
		}
		return &f, nil
	case FrameReqCancel:
		var f ReqCancel
		if err := json.Unmarshal(msg, &f); err != nil {
			return nil, err
		}
		return &f, nil
	case FrameWsOpen:
		var f WsOpen
		if err := json.Unmarshal(msg, &f); err != nil {
			return nil, err
		}
		return &f, nil
	case FrameWsAck:
		var f WsAck
		if err := json.Unmarshal(msg, &f); err != nil {
			return nil, err
		}
		return &f, nil
	case FrameWsFrame:
		var f WsFrame
		if err := json.Unmarshal(msg, &f); err != nil {
			return nil, err
		}
		if len(f.Data) > maxFrameBytes {
			return nil, ErrFrameTooLarge
		}
		return &f, nil
	case FrameWsClose:
		var f WsClose
		if err := json.Unmarshal(msg, &f); err != nil {
			return nil, err
		}
		return &f, nil
	default:
		return nil, ErrUnknownFrame
	}
}
