package bmc

import (
	"errors"
	"testing"
)

// TestClassifyPrecondition pins the 412 mapping: an If-Match/ETag race with
// concurrent sessions (KVM/console page on the same iBMC) is a protocol
// precondition failure — retryable — never "unreachable" (the old default
// bucket, whose label sent operators chasing the network).
func TestClassifyPrecondition(t *testing.T) {
	e := Classify("set_boot_device", errors.New("412: "))
	if e.Kind != KindProtocolError {
		t.Errorf("kind = %s, want BMC_PROTOCOL_ERROR", e.Kind)
	}
	if !e.Retryable() {
		t.Error("etag race must stay retryable")
	}

	e = Classify("set_boot_device", errors.New("PATCH failed: 412 Precondition Failed"))
	if e.Kind != KindProtocolError {
		t.Errorf("kind = %s, want BMC_PROTOCOL_ERROR", e.Kind)
	}

	// The old default still holds for genuinely opaque errors.
	if e := Classify("sel", errors.New("connection refused to 192.168.1.74")); e.Kind != KindUnreachable {
		t.Errorf("kind = %s, want BMC_UNREACHABLE", e.Kind)
	}
}
