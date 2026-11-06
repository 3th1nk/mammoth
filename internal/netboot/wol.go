package netboot

import (
	"encoding/json"
	"fmt"
	"net"
	"time"
)

// Wake-on-LAN (docs/13-headless-design.md §4-5): the AMD magic packet —
// six 0xFF bytes followed by the target MAC sixteen times — broadcast on
// the machine's L2. The headless boot stage uses it as the powered-off
// path's wake-up before falling back to a manual power-on; the edge
// relays the same packet for cross-segment deployments (the engine is not
// on the machine's L2, the proxy is).
const (
	// wolPort is the discard-ish conventional WoL port; gateware only
	// inspects the payload, so the port is arbitrary.
	wolPort = 9
	// wolAttempts is the resend count: UDP broadcasts are fire-and-forget,
	// NICs in deep sleep miss single packets.
	wolAttempts = 3
	// wolRetryGap spaces the resends (some NICs need one to latch the
	// pattern between power states).
	wolRetryGap = 150 * time.Millisecond
)

// MagicPacket builds the 102-byte AMD magic packet for mac (normalized or
// any common separator spelling). Malformed MACs are a programming error at
// the call sites (the registry stores normalized MACs) and yield an error.
func MagicPacket(mac string) ([]byte, error) {
	hw, err := net.ParseMAC(mac)
	if err != nil {
		return nil, fmt.Errorf("wol: %q is not a hardware address", mac)
	}
	if len(hw) != 6 {
		return nil, fmt.Errorf("wol: %q is not a 6-byte MAC", mac)
	}
	packet := make([]byte, 6+16*len(hw))
	for i := 0; i < 6; i++ {
		packet[i] = 0xFF
	}
	for i := 0; i < 16; i++ {
		copy(packet[6+i*6:], hw)
	}
	return packet, nil
}

// SendWoL broadcasts the magic packet to bcast (an L2 broadcast address
// such as "192.168.1.255", or a unicast reachable address — useful in
// tests), resending wolAttempts times. The error is the LAST send's: any
// successful attempt means the packet is on the wire and the wake is the
// NIC's business.
func SendWoL(bcast, mac string) error {
	packet, err := MagicPacket(mac)
	if err != nil {
		return err
	}
	addr := &net.UDPAddr{IP: net.ParseIP(bcast), Port: wolPort}
	conn, err := net.DialUDP("udp", nil, addr)
	if err != nil {
		return fmt.Errorf("wol: dial %s: %w", bcast, err)
	}
	defer conn.Close()
	var last error
	for i := 0; i < wolAttempts; i++ {
		if i > 0 {
			time.Sleep(wolRetryGap)
		}
		if _, err := conn.Write(packet); err != nil {
			last = err
		} else {
			last = nil
		}
	}
	if last != nil {
		return fmt.Errorf("wol: send to %s: %w", bcast, last)
	}
	return nil
}

// WoLRequest is the edge relay endpoint's body.
type WoLRequest struct {
	MAC string `json:"mac"`
}

// ParseWoLRequest decodes and validates the relay request body, returning
// the normalized MAC.
func ParseWoLRequest(body []byte) (string, error) {
	var req WoLRequest
	if err := json.Unmarshal(body, &req); err != nil {
		return "", fmt.Errorf("wol: body is not JSON")
	}
	mac := NormalizeMAC(req.MAC)
	if len(mac) != 17 {
		return "", fmt.Errorf("wol: %q is not a hardware address", req.MAC)
	}
	return mac, nil
}
