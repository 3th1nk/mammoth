package avct

// X11 keysym → USB HID Keyboard/Keypad usage mapping (page 0x07), the pair
// of code spaces between RFB KeyEvent and the appliance's keyboard packets.
// Shifted characters arrive as their base key plus a separate Shift keysym,
// so the table only needs unshifted keys.

// USB HID usage ids (subset the console needs).
const (
	hidA      uint16 = 0x04
	hidZ      uint16 = 0x1D
	hidOne    uint16 = 0x1E
	hidZero   uint16 = 0x27
	hidEnter  uint16 = 0x28
	hidEsc    uint16 = 0x29
	hidBSpace uint16 = 0x2A
	hidTab    uint16 = 0x2B
	hidSpace  uint16 = 0x2C
	hidMinus  uint16 = 0x2D
	hidEqual  uint16 = 0x2E
	hidLBrack uint16 = 0x2F
	hidRBrack uint16 = 0x30
	hidBSlash uint16 = 0x31
	hidTilde  uint16 = 0x32
	hidSemi   uint16 = 0x33
	hidQuote  uint16 = 0x34
	hidGrave  uint16 = 0x35
	hidComma  uint16 = 0x36
	hidDot    uint16 = 0x37
	hidSlash  uint16 = 0x38
	hidCaps   uint16 = 0x39
	hidF1     uint16 = 0x3A
	hidF12    uint16 = 0x45
	hidPrtSc  uint16 = 0x46
	hidInsert uint16 = 0x49
	hidHome   uint16 = 0x4A
	hidPgUp   uint16 = 0x4B
	hidDelete uint16 = 0x4C
	hidEnd    uint16 = 0x4D
	hidPgDown uint16 = 0x4E
	hidRight  uint16 = 0x4F
	hidLeft   uint16 = 0x50
	hidDown   uint16 = 0x51
	hidUp     uint16 = 0x52

	hidLeftCtrl  uint16 = 0xE0
	hidLeftShift uint16 = 0xE1
	hidLeftAlt   uint16 = 0xE2
	hidLeftGUI   uint16 = 0xE3
	hidRightCtrl uint16 = 0xE4
	hidRightShft uint16 = 0xE5
	hidRightAlt  uint16 = 0xE6
	hidRightGUI  uint16 = 0xE7
)

// keysymUsage maps the X11 keysyms noVNC sends for a US layout.
var keysymUsage = map[uint32]uint16{
	// ASCII printable range maps linearly below; specials first.
	0xFF08: hidBSpace, // BackSpace
	0xFF09: hidTab,
	0xFF0A: hidTab, // Linefeed → Tab (viewer convention)
	0xFF0D: hidEnter,
	0xFF13: 0x46, // Pause
	0xFF14: 0x47, // Scroll Lock
	0xFF1B: hidEsc,
	0xFF50: hidHome,
	0xFF51: hidLeft,
	0xFF52: hidUp,
	0xFF53: hidRight,
	0xFF54: hidDown,
	0xFF55: hidPgUp,
	0xFF56: hidPgDown,
	0xFF57: hidEnd,
	0xFF63: hidInsert,
	0xFF7F: 0x53, // Num Lock
	0xFF80: 0x54, // KP_Slash
	0xFFAA: 0x55, // KP_Multiply
	0xFFAB: 0x56, // KP_Minus
	0xFFAD: 0x57, // KP_Plus
	0xFFAE: hidEnter,
	0xFFBE: hidF1,
	0xFFBF: hidF1 + 1,
	0xFFC0: hidF1 + 2,
	0xFFC1: hidF1 + 3,
	0xFFC2: hidF1 + 4,
	0xFFC3: hidF1 + 5,
	0xFFC4: hidF1 + 6,
	0xFFC5: hidF1 + 7,
	0xFFC6: hidF1 + 8,
	0xFFC7: hidF1 + 9,
	0xFFC8: hidF12,
	0xFFC9: hidF12 + 1,
	0xFFCA: hidF12 + 2,
	0xFFCB: hidF12 + 3,
	0xFFCC: hidF12 + 4,
	0xFFCD: hidF12 + 5,
	0xFFE1: hidLeftShift,
	0xFFE2: hidRightShft,
	0xFFE3: hidLeftCtrl,
	0xFFE4: hidRightCtrl,
	0xFFE5: hidCaps,
	0xFFE7: hidLeftGUI,
	0xFFE8: hidRightGUI,
	0xFFE9: hidLeftAlt,
	0xFFEA: hidRightAlt,
	0xFFEB: hidLeftGUI,
	0xFFEC: hidRightGUI,
	0xFFFF: hidDelete, // Delete on some servers
	// ISO/extra symbols
	0x00A1: hidOne,     // exclamdown — approximate
	0x00BC: hidOne + 5, // onequarter — approximate
}

// Note: the X11 keysym space overlaps the keypad digits with F1–F12
// (0xFFB9..0xFFC2 cover both); the function keys win, matching the viewer.

// HID usage ids for the printable ASCII range (the base, unshifted key).
var asciiUsage = func() [128]uint16 {
	var m [128]uint16
	// Letters: a-z → 0x04..0x1D
	for i := 0; i < 26; i++ {
		m['a'+i] = hidA + uint16(i)
	}
	// Digits: 1..9 → 0x1E..0x26, 0 → 0x27
	for i := 0; i < 9; i++ {
		m['1'+i] = hidOne + uint16(i)
	}
	m['0'] = hidZero
	m[' '] = hidSpace
	m['\r'] = hidEnter
	m['\n'] = hidEnter
	m['\t'] = hidTab
	m['\x1b'] = hidEsc
	m['\x08'] = hidBSpace
	m['-'] = hidMinus
	m['='] = hidEqual
	m['['] = hidLBrack
	m[']'] = hidRBrack
	m['\\'] = hidBSlash
	m[';'] = hidSemi
	m['\''] = hidQuote
	m[','] = hidComma
	m['.'] = hidDot
	m['/'] = hidSlash
	return m
}()

// KeysymToUsage resolves an X11 keysym to its USB HID usage.
func KeysymToUsage(keysym uint32) (uint16, bool) {
	if u, ok := keysymUsage[keysym]; ok {
		return u, true
	}
	// Printable ASCII keysyms (0x20..0x7E) carry their own code — for the
	// shifted punctuation we map to the base key: the appliance keys the
	// usage; the Shift modifier rides separately from the client.
	if keysym >= 0x20 && keysym <= 0x7E {
		ch := byte(keysym)
		switch ch {
		case '!':
			return hidOne, true
		case '@':
			return hidOne + 1, true
		case '#':
			return hidOne + 2, true
		case '$':
			return hidOne + 3, true
		case '%':
			return hidOne + 4, true
		case '^':
			return hidOne + 5, true
		case '&':
			return hidOne + 6, true
		case '*':
			return hidOne + 7, true
		case '(':
			return hidOne + 8, true
		case ')':
			return hidZero, true
		case '_':
			return hidMinus, true
		case '+':
			return hidEqual, true
		case '{':
			return hidLBrack, true
		case '}':
			return hidRBrack, true
		case '|':
			return hidBSlash, true
		case ':':
			return hidSemi, true
		case '"':
			return hidQuote, true
		case '~':
			return hidGrave, true
		case '<':
			return hidComma, true
		case '>':
			return hidDot, true
		case '?':
			return hidSlash, true
		}
		if u := asciiUsage[ch]; u != 0 {
			return u, true
		}
		// Upper-case letters share the base usage.
		if ch >= 'A' && ch <= 'Z' {
			return hidA + uint16(ch-'A'), true
		}
	}
	return 0, false
}
