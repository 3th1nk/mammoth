package builder

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
	"github.com/ProtonMail/go-crypto/openpgp/packet"
)

// The offline install pool is an unsigned mirror by construction: the distro
// ISO layout ships a bare suite Release (Debian 13+ has no detached
// Release.gpg at all), while the installer's apt-setup verifies the mirror
// with a full `apt-get update` — signature check included, which the
// allow_unauthenticated preseed does NOT cover. mammoth therefore carries its
// own pool signing key: every staged pool Release is re-signed (armored,
// detached — binary signatures are rejected by current apt) and the matching
// public key rides the preseed early_command into the installer's apt
// trustdb. The key has no trust boundary beyond "this mammoth signed this
// pool", so it is generated on first use and kept under MediaDir.

// poolKeyName/… are the on-disk artifacts under MediaDir.
const (
	poolKeyFile = ".pool-signing-key.asc"
	poolKeyName  = "mammoth offline pool"
	poolKeyEmail = "pool@mammoth.invalid"
)

// PoolSigningEntity loads the pool signing key from MediaDir, generating and
// persisting a fresh one (RSA 3072, sign-only) on first use. The private key
// file is created 0600; MediaDir is mammoth-managed state.
func PoolSigningEntity(mediaDir string) (*openpgp.Entity, error) {
	path := filepath.Join(mediaDir, poolKeyFile)
	if b, err := os.ReadFile(path); err == nil {
		el, err := openpgp.ReadArmoredKeyRing(bytes.NewReader(b))
		if err != nil || len(el) == 0 {
			return nil, errors.New("builder: pool signing key unreadable (regenerate by deleting " + path + ")")
		}
		return el[0], nil
	}
	if err := os.MkdirAll(mediaDir, 0o755); err != nil {
		return nil, err
	}
	now := time.Now()
	ent, err := openpgp.NewEntity(poolKeyName, "", poolKeyEmail, &packet.Config{
		RSABits: 3072, Algorithm: packet.PubKeyAlgoRSA, Time: func() time.Time { return now },
	})
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	w, err := armor.Encode(&buf, openpgp.PrivateKeyType, nil)
	if err != nil {
		return nil, err
	}
	if err := ent.SerializePrivate(w, nil); err != nil {
		return nil, err
	}
	if err := w.Close(); err != nil {
		return nil, err
	}
	if err := os.WriteFile(path, buf.Bytes(), 0o600); err != nil {
		return nil, err
	}
	return ent, nil
}

// PoolPublicKey exports the pool signing key's public part in the binary
// OpenPGP keyring form apt consumes (/etc/apt/trusted.gpg.d entries).
func PoolPublicKey(ent *openpgp.Entity) ([]byte, error) {
	var buf bytes.Buffer
	if err := ent.Serialize(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// SignReleaseDetached writes an ASCII-armored detached signature of data to
// dest (the pool's Release.gpg — armored because current apt rejects binary
// detached signatures).
func SignReleaseDetached(ent *openpgp.Entity, data []byte, dest string) error {
	var sig bytes.Buffer
	w, err := armor.Encode(&sig, "PGP SIGNATURE", nil)
	if err != nil {
		return err
	}
	if err := openpgp.DetachSign(w, ent, bytes.NewReader(data), nil); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return os.WriteFile(dest, sig.Bytes(), 0o644)
}
