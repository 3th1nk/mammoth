package avct

import "log"

// sampleLog is the package-level fallback logger for decode warnings.
// High-frequency paths never log per-packet; these lines are per-bad-packet
// and rate-limited by their nature. The KVM bridge injects real loggers on
// the Session; decoder warnings stay package-scoped to keep the decoder
// signatures small.
var sampleLog = log.Default()

// SetLogger replaces the package logger (wired from the engine's obs logger
// at startup).
func SetLogger(l *log.Logger) {
	if l != nil {
		sampleLog = l
	}
}
