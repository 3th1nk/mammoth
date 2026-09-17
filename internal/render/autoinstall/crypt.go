package autoinstall

import (
	"crypto/rand"

	"github.com/GehirnInc/crypt/sha512_crypt"
)

// subiquity's autoinstall identity.password is a crypt(3) hash: a plain
// string is stored verbatim in /etc/shadow and no login can ever match it
// (2288H real-hardware: a fully green install produced unreachable users).
// Render the one-time passwords as SHA512-crypt ($6$, 5000 rounds) — the
// format useradd consumes everywhere. GehirnInc/crypt is the reference
// faithful pure-Go implementation; the spec's published vector pins it.

// cryptSHA512 renders "$6$<salt>$<hash>" (5000 rounds), salt ≤16 chars.
func cryptSHA512(password, salt string) string {
	c := sha512_crypt.New()
	out, err := c.Generate([]byte(password), []byte("$6$"+salt))
	if err != nil {
		panic("autoinstall: sha512-crypt generate: " + err.Error())
	}
	return out
}

// randomSalt draws a crypt alphabet salt (16 chars — the $6$ default max).
func randomSalt() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	for i := range b {
		b[i] = b64Alphabet[int(b[i])%64]
	}
	return string(b)
}

const b64Alphabet = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
