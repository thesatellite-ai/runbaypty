// codec.go — frame encoder/decoder over any io.Reader/io.Writer.
//
// The encoder assembles each frame into one buffer and issues ONE Write —
// combined with the Encoder mutex this guarantees frames from concurrent
// goroutines never interleave on the wire. The decoder reads with
// io.ReadFull so fragmented transports (1-byte-at-a-time readers, TCP
// segmentation) decode identically to a single contiguous read.
package proto

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"sync"

	"github.com/thesatellite-ai/runbaypty/pkg/errcodes"
)

// Frame is one wire unit: a type, a JSON header, and an optional raw payload.
// Header is the marshaled JSON of the type's message struct (messages.go);
// Payload is raw bytes only for INPUT/OUTPUT (nil otherwise).
type Frame struct {
	Type    FrameType
	Header  []byte
	Payload []byte
}

// NewFrame marshals msg into a Frame of the given type. payload may be nil.
func NewFrame(t FrameType, msg any, payload []byte) (Frame, error) {
	h, err := json.Marshal(msg)
	if err != nil {
		return Frame{}, fmt.Errorf("proto: marshal %s header: %w", t, err)
	}
	if len(h) > MaxHeaderLen {
		return Frame{}, errcodes.Newf(errcodes.FrameTooLarge, "%s header %d bytes exceeds %d", t, len(h), MaxHeaderLen)
	}
	return Frame{Type: t, Header: h, Payload: payload}, nil
}

// DecodeHeader unmarshals the frame's JSON header into out (a pointer to the
// type's message struct). Unknown JSON fields are ignored — additive
// forward-compat with newer peers.
func (f Frame) DecodeHeader(out any) error {
	if err := json.Unmarshal(f.Header, out); err != nil {
		return errcodes.Newf(errcodes.BadFrame, "%s header: %v", f.Type, err).WithCause(err)
	}
	return nil
}

// wireLen returns the value of the u32 length prefix for this frame.
func (f Frame) wireLen() int {
	return frameOverhead + len(f.Header) + len(f.Payload)
}

// AppendWire appends the full wire encoding (including the length prefix)
// to dst and returns the extended slice. Exposed for zero-extra-copy callers
// (the WS listener hands the result straight to the socket library).
func (f Frame) AppendWire(dst []byte) ([]byte, error) {
	if len(f.Header) > MaxHeaderLen {
		return dst, errcodes.Newf(errcodes.FrameTooLarge, "header %d bytes exceeds %d", len(f.Header), MaxHeaderLen)
	}
	total := f.wireLen()
	if total > MaxFrameLen {
		return dst, errcodes.Newf(errcodes.FrameTooLarge, "frame %d bytes exceeds %d", total, MaxFrameLen)
	}
	dst = binary.BigEndian.AppendUint32(dst, uint32(total))
	dst = append(dst, byte(f.Type))
	dst = binary.BigEndian.AppendUint16(dst, uint16(len(f.Header)))
	dst = append(dst, f.Header...)
	dst = append(dst, f.Payload...)
	return dst, nil
}

// Encoder serializes frames onto w. Safe for concurrent use: the internal
// mutex plus single-Write assembly guarantee no interleaving.
type Encoder struct {
	mu  sync.Mutex
	w   io.Writer
	buf []byte // reused between frames; grows to the largest frame seen
}

// NewEncoder returns an Encoder writing to w.
func NewEncoder(w io.Writer) *Encoder {
	return &Encoder{w: w}
}

// Write encodes one frame and writes it in a single w.Write call.
func (e *Encoder) Write(f Frame) error {
	e.mu.Lock()
	defer e.mu.Unlock()
	var err error
	e.buf, err = f.AppendWire(e.buf[:0])
	if err != nil {
		return err
	}
	if _, err := e.w.Write(e.buf); err != nil {
		return fmt.Errorf("proto: write %s frame: %w", f.Type, err)
	}
	return nil
}

// WriteMsg is the NewFrame + Write convenience.
func (e *Encoder) WriteMsg(t FrameType, msg any, payload []byte) error {
	f, err := NewFrame(t, msg, payload)
	if err != nil {
		return err
	}
	return e.Write(f)
}

// Decoder reads frames from r. NOT safe for concurrent use — one reader
// goroutine per connection is the model.
type Decoder struct {
	r    io.Reader
	head [4]byte
}

// NewDecoder returns a Decoder reading from r.
func NewDecoder(r io.Reader) *Decoder {
	return &Decoder{r: r}
}

// Read decodes the next frame. It returns:
//   - io.EOF on a clean close at a frame boundary
//   - io.ErrUnexpectedEOF when the peer died mid-frame
//   - E_FRAME_TOO_LARGE before allocating anything for an oversized length
//   - E_BAD_FRAME for structurally invalid length/header accounting
//
// Unknown frame TYPES decode successfully — skipping them (with a warning)
// is the dispatcher's choice, never a connection kill (additive compat).
func (d *Decoder) Read() (Frame, error) {
	if _, err := io.ReadFull(d.r, d.head[:]); err != nil {
		if err == io.EOF {
			return Frame{}, io.EOF // clean close between frames
		}
		return Frame{}, fmt.Errorf("proto: read length: %w", err)
	}
	total := int(binary.BigEndian.Uint32(d.head[:]))
	if total > MaxFrameLen {
		return Frame{}, errcodes.Newf(errcodes.FrameTooLarge, "frame %d bytes exceeds %d", total, MaxFrameLen)
	}
	if total < frameOverhead {
		return Frame{}, errcodes.Newf(errcodes.BadFrame, "frame length %d below minimum %d", total, frameOverhead)
	}
	body := make([]byte, total)
	if _, err := io.ReadFull(d.r, body); err != nil {
		return Frame{}, fmt.Errorf("proto: read body: %w", err)
	}
	headerLen := int(binary.BigEndian.Uint16(body[1:3]))
	if frameOverhead+headerLen > total {
		return Frame{}, errcodes.Newf(errcodes.BadFrame, "header length %d exceeds frame body %d", headerLen, total-frameOverhead)
	}
	return Frame{
		Type:    FrameType(body[0]),
		Header:  body[frameOverhead : frameOverhead+headerLen],
		Payload: body[frameOverhead+headerLen:],
	}, nil
}
