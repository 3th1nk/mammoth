package h3c

import (
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"time"
)

// Paste injection for the RFB passthrough bridge: the HDM's VNC service has
// no cut-text consumer either, so the bridge intercepts noVNC's
// ClientCutText and types the text out as standard RFB KeyEvents. Unlike
// the avct/Dell path (paste.go there) no Shift synthesis is needed — the
// keysym carries the case itself and the HDM decodes it against the host's
// keyboard layout, which for command lines is US anyway.

const (
	pasteKeyHold = 10 * time.Millisecond // press→release gap
	pastePace    = 25 * time.Millisecond // between keys — mirrors the avct injector's proven pacing
	pasteEnterP  = 60 * time.Millisecond // extra settle after Enter

	// pasteMaxBytes caps what the injector will type (the avct injector
	// uses the same cap); at ~28 chars/s a bigger payload would hold the
	// single-flight slot for absurd spans.
	pasteMaxBytes = 64 << 10
)

type logger interface {
	Printf(format string, args ...any)
}

// pasteState is the bridge's single-flight paste slot. The filter
// goroutine (start/cancelNow) and the injector goroutine (run) are the
// two parties, same shape as avct's rfbClient paste fields.
type pasteState struct {
	dev   net.Conn
	devMu *sync.Mutex // serialises device writes with the filter's forwards
	log   logger

	mu     sync.Mutex
	cancel chan struct{} // non-nil while an injection is running
}

// start claims the slot and spawns the injector. Single flight: while one
// runs, further ones are ignored — the operator cancels by pressing any
// key (the filter does that on a real KeyEvent press).
func (p *pasteState) start(text []byte) {
	if len(text) == 0 {
		return
	}
	if len(text) > pasteMaxBytes {
		p.log.Printf("rfb: paste: %d bytes over the %d cap, ignored", len(text), pasteMaxBytes)
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		p.log.Printf("rfb: paste: one already running, ignored %d bytes", len(text))
		return
	}
	cancel := make(chan struct{})
	p.cancel = cancel
	go p.run(text, cancel)
}

// cancelNow stops the running injection, if any.
func (p *pasteState) cancelNow() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cancel != nil {
		close(p.cancel)
		p.cancel = nil
	}
}

// run types the text at pastePace. Only lengths land in the log — pasted
// content is operator input, not telemetry.
func (p *pasteState) run(text []byte, cancel chan struct{}) {
	defer func() {
		p.mu.Lock()
		if p.cancel == cancel {
			p.cancel = nil
		}
		p.mu.Unlock()
	}()
	log := p.log

	typed, skipped := 0, 0
	runes := normalizePaste(text)
	for _, r := range runes {
		sym, ok := pasteKeysym(r)
		if !ok {
			skipped++
			continue
		}
		// down, hold, up — the up always goes out, even on cancel, so the
		// host never sees a stuck key.
		if err := p.writeKey(sym, true); err != nil {
			log.Printf("rfb: paste: key down: %v", err)
			return
		}
		if !wait(cancel, pasteKeyHold) {
			_ = p.writeKey(sym, false)
			log.Printf("rfb: paste: canceled after %d/%d chars", typed, len(runes))
			return
		}
		if err := p.writeKey(sym, false); err != nil {
			log.Printf("rfb: paste: key up: %v", err)
			return
		}
		typed++
		if !wait(cancel, pastePaceAfter(r)) {
			log.Printf("rfb: paste: canceled after %d/%d chars", typed, len(runes))
			return
		}
	}
	log.Printf("rfb: paste: typed %d chars, skipped %d (of %d bytes)", typed, skipped, len(text))
}

// writeKey sends one RFB KeyEvent half under the device write lock — the
// filter forwards other client messages on the same connection, and RFB
// messages must not interleave.
func (p *pasteState) writeKey(sym uint32, down bool) error {
	p.devMu.Lock()
	defer p.devMu.Unlock()
	_, err := p.dev.Write(keyEventFrame(sym, down))
	return err
}

func keyEventFrame(sym uint32, down bool) []byte {
	b := []byte{4, 0, 0, 0}
	if down {
		b[1] = 1
	}
	return binary.BigEndian.AppendUint32(b, sym)
}

func wait(cancel <-chan struct{}, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-cancel:
		return false
	case <-t.C:
		return true
	}
}

// normalizePaste folds CRLF and lone CR into LF so a Windows-copied command
// does not double-Enter (same rule as the avct injector).
func normalizePaste(text []byte) []rune {
	s := strings.ReplaceAll(string(text), "\r\n", "\n")
	s = strings.ReplaceAll(s, "\r", "\n")
	return []rune(s)
}

// pasteKeysym maps one pasted rune to an RFB keysym. Control characters
// beyond Tab and Newline have no key and are skipped; runes above ASCII
// are skipped too (noVNC rewrites non-Latin-1 to '?' on the wire — typing
// those would litter question marks, so the viewer strips them before
// sending).
func pasteKeysym(r rune) (uint32, bool) {
	switch r {
	case '\n':
		return 0xFF0D, true // Enter
	case '\t':
		return 0xFF09, true // Tab
	}
	if r < 0x20 || r > 0x7E {
		return 0, false
	}
	return uint32(r), true
}

// pastePaceAfter is the inter-key pause; a fresh Enter gets extra settle
// time because the shell on the other side is slower than the key queue.
func pastePaceAfter(r rune) time.Duration {
	if r == '\n' {
		return pasteEnterP
	}
	return pastePace
}
