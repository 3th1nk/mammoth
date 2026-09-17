package autoinstall

import (
	"strings"
	"testing"
)

// The published sha512crypt test vector — pins the permutation, the round
// loop, and the P/S buffer construction in one shot. Any drift here ships
// passwords nobody can type.
func TestCryptSHA512SpecVector(t *testing.T) {
	got := cryptSHA512("Hello world!", "saltstring")
	want := "$6$saltstring$svn8UoSVapNtMuq1ukKS4tPQd8iKwSMHWjl/O817G3uBnIFNjnQJuesI68u4OTLiBFdcbYEdFCoEOfaS35inz1"
	if got != want {
		t.Fatalf("crypt mismatch:\n got %s\nwant %s", got, want)
	}
}

func TestRandomSaltShape(t *testing.T) {
	s := randomSalt()
	if len(s) != 16 {
		t.Fatalf("salt len %d", len(s))
	}
	if strings.ContainsAny(s, "\x00") || strings.Contains(s, "$") {
		t.Fatalf("salt alphabet off: %q", s)
	}
	if s == randomSalt() {
		t.Fatal("salt not random")
	}
}
