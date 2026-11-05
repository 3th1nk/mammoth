package netboot

import (
	"bytes"
	"encoding/json"
	"testing"
)

// TestMagicPacketGolden pins the AMD magic packet shape: six 0xFF bytes
// followed by the target MAC sixteen times (102 bytes for a 6-byte MAC).
// Sleepy NICs match the exact byte pattern — nothing else wakes them.
func TestMagicPacketGolden(t *testing.T) {
	packet, err := MagicPacket("52:54:00:12:34:56")
	if err != nil {
		t.Fatalf("MagicPacket: %v", err)
	}
	if len(packet) != 102 {
		t.Fatalf("packet length = %d, want 102", len(packet))
	}
	if !bytes.Equal(packet[:6], []byte{0xFF, 0xFF, 0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Fatalf("sync prefix wrong: %x", packet[:6])
	}
	for i := 0; i < 16; i++ {
		want := []byte{0x52, 0x54, 0x00, 0x12, 0x34, 0x56}
		if !bytes.Equal(packet[6+i*6:12+i*6], want) {
			t.Fatalf("repetition %d wrong: %x", i, packet[6+i*6:12+i*6])
		}
	}
	if _, err := MagicPacket("not-a-mac"); err == nil {
		t.Fatal("malformed MAC must error")
	}
}

// SendWoL to the loopback (a unicast-reachable address — the send path is
// identical, only the destination differs): three attempts, last-write
// wins, no error.
func TestSendWoLLoopback(t *testing.T) {
	if err := SendWoL("127.0.0.1", "52:54:00:12:34:56"); err != nil {
		t.Fatalf("SendWoL loopback: %v", err)
	}
}

// TestParseWoLRequest pins the relay endpoint's request validation.
func TestParseWoLRequest(t *testing.T) {
	mac, err := ParseWoLRequest([]byte(`{"mac":"AA-BB-CC-DD-EE-01"}`))
	if err != nil || mac != "aa:bb:cc:dd:ee:01" {
		t.Fatalf("mac = %q err = %v", mac, err)
	}
	if _, err := ParseWoLRequest([]byte(`{"mac":"nonsense"}`)); err == nil {
		t.Fatal("non-MAC must error")
	}
	if _, err := ParseWoLRequest([]byte(`not json`)); err == nil {
		t.Fatal("non-JSON must error")
	}
}

// The engine→proxy contract: the engine marshals WoLRequest, the proxy
// parses it back — one round trip keeps the two sides honest (they live in
// different processes).
func TestWoLRequestRoundTrip(t *testing.T) {
	body, err := json.Marshal(WoLRequest{MAC: "52:54:00:12:34:56"})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	mac, err := ParseWoLRequest(body)
	if err != nil || mac != "52:54:00:12:34:56" {
		t.Fatalf("round trip = %q %v", mac, err)
	}
}
