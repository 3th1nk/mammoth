package avct

import (
	"encoding/binary"
	"fmt"
)

// Control-channel packet types we exchange (BEEF frames). The 0x83xx block
// is server→client, 0x02xx client→server input, 0x03xx/0x04xx control.
const (
	TypeLoginRequest      uint16 = 0x0102 // client: credentials + session id
	TypeKeepAlive         uint16 = 0x0400 // client: 2s heartbeat
	TypeKeyboardData      uint16 = 0x0200 // client: USB HID usage press/release
	TypeMouseData         uint16 = 0x0201 // client: absolute position + buttons + wheel
	TypeVideoEnable       uint16 = 0x030E // client: start video (after connect status)
	TypeSetDisplayArea    uint16 = 0x0302 // client: logical display geometry
	TypeSetScaleMode1to1  uint16 = 0x0304 // client: no scaling
	TypeScreenRefresh     uint16 = 0x0301 // client: request full redraw
	TypeVideoAck          uint16 = 0x0000 // client: video flow control (also on video socket)
	TypeVideoChannelAuth  byte   = 0x01   // video socket client auth (video header)
	TypeLoginResponse     uint16 = 0x8305 // server: login result
	TypeVideoConnStatus   uint16 = 0x0084 // server: video channel connect result (low byte 0x84)
	TypeVideoStopped      uint16 = 0x0085 // server: video stopped (reason)
	TypeDisplayResolution uint16 = 0x8201 // server: current video geometry
	TypeInputResolution   uint16 = 0x8101 // server: input coordinate space + platform tag
	TypeKeyboardLED       uint16 = 0x8100 // server: LED state
	TypeUserDisconnect    uint16 = 0x8301 // server: disconnect notice
	TypeSharingRequest    uint16 = 0x8401 // server: another user wants to share
)

// Login failure status codes carried by the 0x8305 response.
const (
	loginOK              = 0
	loginBadLogin        = 1 // also 2
	loginAccessDenied    = 3
	loginInUse           = 4 // also 12
	loginAllChannelsUsed = 8
	loginSharing         = 15 // also 16
	loginLicenseExpired  = 24
)

// LoginError explains a rejected console login.
type LoginError struct {
	Status  int
	Reason  string
	Message string
}

func (e *LoginError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("avct: login failed: %s (status %d): %s", e.Reason, e.Status, e.Message)
	}
	return fmt.Sprintf("avct: login failed: %s (status %d)", e.Reason, e.Status)
}

// loginReason maps the appliance's status byte.
func loginReason(status int) string {
	switch status {
	case loginOK:
		return "OK"
	case 1, 2:
		return "BAD_LOGIN"
	case loginAccessDenied:
		return "ACCESS_DENIED"
	case loginInUse, 12:
		return "IN_USE"
	case loginAllChannelsUsed:
		return "ALL_CHANNELS_IN_USE"
	case loginSharing, 16:
		return "SHARING"
	case loginLicenseExpired:
		return "LICENCE_EXPIRED"
	default:
		return fmt.Sprintf("STATUS_%d", status)
	}
}

// shareMode values for the login request.
const (
	shareExclusive = 0
	shareShared    = 1
)

const loginFieldWidth = 96

// loginRequestBody encodes the 0x0102 short-format login:
//
//	u8  username length
//	96  username (zero padded)
//	u8  password length
//	96  password (zero padded)
//	8   RIP id (unused, zero)
//	u8  port       (KMPORT & 0xFF when the APCP version ≥ 2, else 1)
//	u8  channel    (launcher CHANNEL param, 0)
//	i32 clientRandom (session id, also echoed in the video auth)
//	u8  shareMode  (present when the negotiated protocol is ≥ 2)
func loginRequestBody(user, pass string, port, channel byte, clientRandom int32, share byte) []byte {
	b := make([]byte, 1+loginFieldWidth+1+loginFieldWidth+8+1+1+4+1)
	b[0] = byte(len(user))
	copy(b[1:], user)
	b[97] = byte(len(pass))
	copy(b[98:], pass)
	b[194], b[195], b[196], b[197], b[198], b[199], b[200], b[201] = 0, 0, 0, 0, 0, 0, 0, 0
	b[202] = port
	b[203] = channel
	binary.BigEndian.PutUint32(b[204:], uint32(clientRandom))
	b[208] = share
	return b
}

// LoginResponse is the decoded 0x8305:
//
//	u8  status (0 = accepted)
//	u8  unknown
//	u8  unknown
//	u8  zero
//	i32 applianceRandom (echoed in the video channel auth)
//	u16 flags: bit0 = video rides the control socket, bit1 = video socket is
//	           direct TLS, bit6 = view-only session
//	u8  message length
//	msg message text
type LoginResponse struct {
	Status          int
	Reason          string
	ApplianceRandom int32
	Flags           uint16
	Message         string
}

// Accepted reports whether the login succeeded.
func (lr *LoginResponse) Accepted() bool { return lr.Status == loginOK }

// VideoOnControl reports whether video frames arrive on the control socket.
func (lr *LoginResponse) VideoOnControl() bool { return lr.Flags&0x01 != 0 }

// DirectSSLVideo reports whether the video socket expects direct TLS.
func (lr *LoginResponse) DirectSSLVideo() bool { return lr.Flags&0x02 != 0 }

// ViewOnly reports a view-only grant (no input forwarding).
func (lr *LoginResponse) ViewOnly() bool { return lr.Flags&0x40 != 0 }

func decodeLoginResponse(body []byte) (*LoginResponse, error) {
	if len(body) < 11 {
		return nil, fmt.Errorf("avct: login response too short (%d)", len(body))
	}
	lr := &LoginResponse{
		Status:          int(body[0]),
		ApplianceRandom: int32(binary.BigEndian.Uint32(body[4:8])),
		Flags:           binary.BigEndian.Uint16(body[8:10]),
	}
	n := int(body[10])
	if n > 0 && 11+n <= len(body) {
		lr.Message = cstring(body[11 : 11+n])
	}
	if !lr.Accepted() {
		lr.Reason = loginReason(lr.Status)
	}
	return lr, nil
}

func cstring(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// keyboardBody encodes 0x0200:
//
//	u8  0
//	u8  release (0 = pressed, 1 = released)
//	u16 USB HID usage id
//	4   zero
func keyboardBody(usage uint16, down bool) []byte {
	rel := byte(1)
	if down {
		rel = 0
	}
	return []byte{0, rel, byte(usage >> 8), byte(usage), 0, 0, 0, 0}
}

// mouseBody encodes 0x0201 (absolute video-pixel coordinates):
//
//	u8  0
//	u8  buttons (bit0 left, bit1 middle, bit2 right)
//	u16 x
//	u16 y
//	i16 wheel (positive = up)
func mouseBody(buttons uint8, x, y int, wheel int) []byte {
	b := make([]byte, 8)
	b[1] = buttons
	binary.BigEndian.PutUint16(b[2:], uint16(clamp0(x)))
	binary.BigEndian.PutUint16(b[4:], uint16(clamp0(y)))
	binary.BigEndian.PutUint16(b[6:], uint16(int16(wheel)))
	return b
}

func clamp0(v int) int {
	if v < 0 {
		return 0
	}
	return v
}

// videoEnableBody encodes 0x030E: {01 01 00 ...} to enable.
func videoEnableBody(enable bool) []byte {
	b := make([]byte, 8)
	if enable {
		b[0] = 1
	}
	b[1] = 1
	return b
}

// displayAreaBody encodes 0x0302 (width/height in video pixels).
func displayAreaBody(w, h int) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint16(b[0:], uint16(w))
	binary.BigEndian.PutUint16(b[2:], uint16(h))
	return b
}

// videoAuthBody encodes the video-channel auth packet (video header type
// 0x01): the client random from the login and the appliance random from the
// login response.
func videoAuthBody(clientRandom, applianceRandom int32) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint32(b[0:], uint32(clientRandom))
	binary.BigEndian.PutUint32(b[4:], uint32(applianceRandom))
	return b
}

// videoAckBody encodes the flow-control packet sent every 20 decoded video
// packets: the count rides the first payload byte.
func videoAckBody(count uint8) []byte {
	b := make([]byte, 8)
	b[0] = count
	return b
}
