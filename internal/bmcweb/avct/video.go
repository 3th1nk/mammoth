package avct

import (
	"encoding/binary"
	"fmt"
)

// VideoStream dispatches decoded video-channel frames into a Framebuffer
// and tracks flow control (the appliance pauses after 20 unacked packets).
// Wire summary (docs/compat/dell.md §视频通道), server→client frames with
// the type in header byte 5:
//
//	0x80 Video Ack echo   0x81 DVC15  0x82 DVC7  0x83 DVC7-gray  0x8A DVC23
//	0x84 Connect Status   0x85 VideoStopped
//	0x86 ASpeed JPEG (iDRAC7+)
//	0x87 text-mode video  0x88 text palette  0x89 font table
type VideoStream struct {
	fb      *Framebuffer
	unacked int
	text    *TextMode
	dvc     map[byte]*dvcDecoder
	aspeed  *aspeedDecoder
}

// NewVideoStream binds the decoders to the framebuffer.
func NewVideoStream(fb *Framebuffer) *VideoStream {
	return &VideoStream{
		fb:     fb,
		text:   NewTextMode(fb),
		dvc:    map[byte]*dvcDecoder{},
		aspeed: newASpeedDecoder(fb),
	}
}

// Feed handles one decoded frame. ackNeeded reports that the packet counts
// toward the 20-packet flow-control window (the caller sends the Video Ack).
func (vs *VideoStream) Feed(f *Frame, ack func(needed bool)) {
	low := byte(f.Type)
	switch low {
	case 0x86:
		if err := vs.aspeed.handle(f.Body); err != nil {
			sampleLog.Printf("avct: aspeed decode: %v", err)
		}
		vs.ackAndNotify(ack)
	case 0x81, 0x82, 0x83, 0x8A:
		dec := vs.dvc[low]
		if dec == nil {
			dec = &dvcDecoder{fb: vs.fb}
			vs.dvc[low] = dec
		}
		if err := dec.decode(f.Body, low); err != nil {
			sampleLog.Printf("avct: dvc decode: %v", err)
		}
		vs.ackAndNotify(ack)
	case 0x87:
		vs.text.Video(f.Body)
		vs.ackAndNotify(ack)
	case 0x88:
		vs.text.Palette(f.Body)
		vs.ackAndNotify(ack)
	case 0x89:
		vs.text.Font(f.Body)
		vs.ackAndNotify(ack)
	case 0x85: // VideoStopped: reason in body[0] (0 recalibrating, 1 no signal, 2 out of range, 3/4 blocked)
		vs.ackAndNotify(ack)
	case 0x20, 0x01: // Display Resolution Response (type byte on the control channel is 0x8201's low byte)
		if w, h := decodeResolution(f.Body); w > 0 && h > 0 {
			vs.fb.Resize(w, h)
		}
		ack(false)
	default:
		ack(false)
	}
}

func (vs *VideoStream) ackAndNotify(ack func(bool)) {
	vs.unacked++
	if vs.unacked >= 20 {
		vs.unacked = 0
		ack(true)
		return
	}
	ack(false)
}

// VideoStoppedReason renders the 0x85 reason byte.
func VideoStoppedReason(n int) string {
	switch n {
	case 0:
		return "recalibrating"
	case 1:
		return "no signal"
	case 2:
		return "out of range"
	case 3, 4:
		return "blocked by administrator"
	default:
		return fmt.Sprintf("reason %d", n)
	}
}

// decodeResolution extracts the 0x8201 display resolution body
// (u16be width, u16be height).
func decodeResolution(body []byte) (w, h int) {
	if len(body) < 4 {
		return 0, 0
	}
	return int(binary.BigEndian.Uint16(body[0:2])), int(binary.BigEndian.Uint16(body[2:4]))
}
