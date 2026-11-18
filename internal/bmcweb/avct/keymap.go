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
	m['`'] = hidGrave
	m[','] = hidComma
	m['.'] = hidDot
	m['/'] = hidSlash
	return m
}()

// shiftedUsage maps shifted ASCII punctuation to its base key; the paste
// injector presses Shift itself around it (the KeyEvent path gets Shift as
// separate keysyms from the client, so it never needed this table).
var shiftedUsage = map[byte]uint16{
	'!': hidOne, '@': hidOne + 1, '#': hidOne + 2, '$': hidOne + 3,
	'%': hidOne + 4, '^': hidOne + 5, '&': hidOne + 6, '*': hidOne + 7,
	'(': hidOne + 8, ')': hidZero, '_': hidMinus, '+': hidEqual,
	'{': hidLBrack, '}': hidRBrack, '|': hidBSlash, ':': hidSemi,
	'"': hidQuote, '~': hidGrave, '<': hidComma, '>': hidDot, '?': hidSlash,
}

// CharUsage maps a printable ASCII char to (base usage, needs shift) —
// the typed-paste face of the keymap: unlike KeyEvent, paste synthesises
// Shift itself, so the two facts must come out separately. Returns usage
// 0 when the char has no key.
func CharUsage(ch byte) (uint16, bool) {
	if u := asciiUsage[ch]; u != 0 {
		return u, false
	}
	if ch >= 'A' && ch <= 'Z' {
		return hidA + uint16(ch-'A'), true
	}
	return shiftedUsage[ch], true
}

// KeysymToUsage resolves an X11 keysym to its USB HID usage. The bool is
// the found-flag, not a shift request: shifted characters arrive as their
// base key plus a separate Shift keysym from the client.
func KeysymToUsage(keysym uint32) (uint16, bool) {
	if u, ok := keysymUsage[keysym]; ok {
		return u, true
	}
	// Printable ASCII keysyms (0x20..0x7E) carry their own code; CharUsage
	// knows the base key (and the shift, which this path ignores).
	if keysym >= 0x20 && keysym <= 0x7E {
		if u, _ := CharUsage(byte(keysym)); u != 0 {
			return u, true
		}
	}
	return 0, false
}
