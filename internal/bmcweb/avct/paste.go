package avct

import (
	"strings"
	"time"
)

// Paste turns a ClientCutText payload into keystrokes through the same
// keyboard pipeline as manual typing: there is no clipboard channel on the
// appliance side — the BMC synthesises USB HID, so "paste" is typed paste.
// The appliance decodes usages as a US keyboard, which is exact for the
// ASCII a command line needs; other runes are skipped.

const (
	pasteKeyHold = 10 * time.Millisecond // press→release gap, keeps the appliance's HID queue honest
	pastePace    = 25 * time.Millisecond // between keys — older iDRACs drop keys when driven faster
	pasteEnterP  = 60 * time.Millisecond // extra settle after Enter, shells are slower than key queues

	// pasteMaxBytes caps what the injector will type: at ~28 chars/s a
	// bigger payload would hold the single-flight slot for absurd spans.
	// The 1 MiB ClientCutText parse limit in rfb.go stays (stream sync).
	pasteMaxBytes = 64 << 10
)

// startPaste launches the injector for one ClientCutText payload. Single
// flight per viewer: while an injection runs, further ones are ignored —
// the operator cancels by pressing any key (readLoop does that) rather
// than queueing a second burst behind the first.
func (c *rfbClient) startPaste(text []byte) {
	if c.bridge.Input == nil || len(text) == 0 {
		return
	}
	if len(text) > pasteMaxBytes {
		c.bridge.Logger.Printf("avct: paste: %d bytes over the %d cap, ignored", len(text), pasteMaxBytes)
		return
	}
	c.pasteMu.Lock()
	defer c.pasteMu.Unlock()
	if c.pasteCancel != nil {
		c.bridge.Logger.Printf("avct: paste: one already running, ignored %d bytes", len(text))
		return
	}
	cancel := make(chan struct{})
	c.pasteCancel = cancel
	go c.pasteLoop(text, cancel)
}

// cancelPaste stops the running injection, if any. Called from readLoop on
// a real key press and when the session is over.
func (c *rfbClient) cancelPaste() {
	c.pasteMu.Lock()
	defer c.pasteMu.Unlock()
	if c.pasteCancel != nil {
		close(c.pasteCancel)
		c.pasteCancel = nil
	}
}

// pasteLoop types the text at pastePace. Only lengths land in the log —
// pasted content is operator input, not telemetry.
func (c *rfbClient) pasteLoop(text []byte, cancel <-chan struct{}) {
	defer func() {
		c.pasteMu.Lock()
		if c.pasteCancel == cancel {
			c.pasteCancel = nil
		}
		c.pasteMu.Unlock()
	}()
	log := c.bridge.Logger
	input := c.bridge.Input

	var held []uint16 // keys the injector itself is holding, released LIFO
	press := func(usage uint16) error {
		held = append(held, usage)
		return input.KeyDown(usage)
	}
	releaseHeld := func() {
		for i := len(held) - 1; i >= 0; i-- {
			_ = input.KeyUp(held[i])
		}
		held = held[:0]
	}
	defer releaseHeld()

	// wait paces the loop and reports cancellation.
	wait := func(d time.Duration) bool {
		t := time.NewTimer(d)
		defer t.Stop()
		select {
		case <-cancel:
			return false
		case <-t.C:
			return true
		}
	}

	typed, skipped := 0, 0
	runes := normalizePaste(text)
	for _, r := range runes {
		usage, shift, ok := pasteCharUsage(r)
		if !ok {
			skipped++
			continue
		}
		if shift {
			if err := press(hidLeftShift); err != nil {
				log.Printf("avct: paste: shift down: %v", err)
				return
			}
		}
		if err := press(usage); err != nil {
			log.Printf("avct: paste: key down: %v", err)
			return
		}
		if !wait(pasteKeyHold) {
			log.Printf("avct: paste: canceled after %d/%d chars", typed, len(runes))
			return
		}
		releaseHeld()
		typed++
		if !wait(pastePaceAfter(r)) {
			log.Printf("avct: paste: canceled after %d/%d chars", typed, len(runes))
			return
		}
	}
	log.Printf("avct: paste: typed %d chars, skipped %d (of %d bytes)", typed, skipped, len(text))
}

// normalizePaste folds CRLF and lone CR into LF so a Windows-copied command
// does not double-Enter.
func normalizePaste(text []byte) []rune {
	s := strings.ReplaceAll(string(text), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return []rune(s)
}

// pasteCharUsage maps one pasted rune to (HID usage, needs shift). Control
// characters beyond Tab and Newline have no key and are skipped; runes
// above ASCII are skipped too (noVNC rewrites non-Latin-1 to '?' on the
// wire — typing those would litter question marks, so the viewer strips
// them before sending).
func pasteCharUsage(r rune) (uint16, bool, bool) {
	switch r {
	case '\n':
		return hidEnter, false, true
	case '\t':
		return hidTab, false, true
	}
	if r < 0x20 || r > 0x7E {
		return 0, false, false
	}
	usage, shift := CharUsage(byte(r))
	if usage == 0 {
		return 0, false, false
	}
	return usage, shift, true
}

// pastePaceAfter is the inter-key pause; a fresh Enter gets extra settle
// time because the shell on the other side is slower than the key queue.
func pastePaceAfter(r rune) time.Duration {
	if r == '\n' {
		return pasteEnterP
	}
	return pastePace
}
