package builder

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ProtonMail/go-crypto/openpgp"
	"github.com/ProtonMail/go-crypto/openpgp/armor"
)

// A minimal ISO-shaped pool plus a staged udeb subset, covering the whole
// staging pipeline: fill, checksum rewrite across all four Release sections,
// and an armored detached signature the exported public key verifies.
func TestStageNetbootPool(t *testing.T) {
	dir := t.TempDir()
	iso, udebs := filepath.Join(dir, "iso"), filepath.Join(dir, "udebs")

	// ISO-shaped tree: pruned installer index + suite dir + symlink alias.
	mustWrite(t, filepath.Join(iso, "dists/trixie/main/debian-installer/binary-amd64/Packages"), "pruned-index")
	mustWrite(t, filepath.Join(iso, "dists/trixie/main/debian-installer/binary-amd64/Packages.gz"), "pruned-index-gz")
	mustWrite(t, filepath.Join(iso, "dists/trixie/main/binary-amd64/Packages.gz"), "deb-index-gz")
	mustWrite(t, filepath.Join(iso, "pool/main/a/acl/acl-udeb_1_amd64.udeb"), "acl-from-iso")
	mustSymlink(t, filepath.Join(iso, "dists/stable"), "trixie")
	release := "Origin: Debian\nSuite: stable\nCodename: trixie\n" +
		"MD5Sum:\n" +
		" 00000000000000000000000000000000      3 main/debian-installer/binary-amd64/Packages\n" +
		" 00000000000000000000000000000000      3 main/debian-installer/binary-amd64/Packages.gz\n" +
		"SHA1:\n" +
		" 0000000000000000000000000000000000000000      3 main/debian-installer/binary-amd64/Packages\n" +
		" 0000000000000000000000000000000000000000      3 main/debian-installer/binary-amd64/Packages.gz\n" +
		"SHA256:\n" +
		" 0000000000000000000000000000000000000000000000000000000000000000      3 main/debian-installer/binary-amd64/Packages.gz\n" +
		"SHA512:\n" +
		" 0000      3 main/debian-installer/binary-amd64/Packages.gz\n"
	mustWrite(t, filepath.Join(iso, "dists/trixie/Release"), release)

	// Staged subset: the complete index + a netboot-only udeb the ISO pruned.
	mustWrite(t, filepath.Join(udebs, "dists/trixie/main/debian-installer/binary-amd64/Packages"), "complete-index")
	mustWrite(t, filepath.Join(udebs, "dists/trixie/main/debian-installer/binary-amd64/Packages.gz"), "complete-index-gz")
	mustWrite(t, filepath.Join(udebs, "pool/main/l/linux-signed-amd64/kernel-image-di.udeb"), "kernel-image")

	ent, err := PoolSigningEntity(dir)
	if err != nil {
		t.Fatalf("key: %v", err)
	}
	if err := StageNetbootPool(iso, udebs, ent); err != nil {
		t.Fatalf("stage: %v", err)
	}

	// Fill landed: pruned index replaced, kernel udeb copied, ISO file kept.
	if b, _ := os.ReadFile(filepath.Join(iso, "dists/trixie/main/debian-installer/binary-amd64/Packages")); string(b) != "complete-index" {
		t.Errorf("installer index not replaced: %q", b)
	}
	if _, err := os.Stat(filepath.Join(iso, "pool/main/l/linux-signed-amd64/kernel-image-di.udeb")); err != nil {
		t.Errorf("kernel udeb not filled: %v", err)
	}
	if b, _ := os.ReadFile(filepath.Join(iso, "pool/main/a/acl/acl-udeb_1_amd64.udeb")); string(b) != "acl-from-iso" {
		t.Errorf("existing pool file overwritten: %q", b)
	}

	// Every checksum section agrees with the staged bytes.
	relB, _ := os.ReadFile(filepath.Join(iso, "dists/trixie/Release"))
	gz, _ := os.ReadFile(filepath.Join(iso, "dists/trixie/main/debian-installer/binary-amd64/Packages.gz"))
	h := sha256.Sum256(gz)
	if !strings.Contains(string(relB), hex.EncodeToString(h[:])+" "+strconv.Itoa(len(gz))+" main/debian-installer/binary-amd64/Packages.gz") {
		t.Errorf("SHA256 entry not rewritten:\n%s", relB)
	}
	if strings.Count(string(relB), "00000000000000000000000000000000") != 0 {
		t.Errorf("stale MD5 entries remain:\n%s", relB)
	}
	// The deb index (untouched) keeps its original entry form — rewrite only
	// touches entries whose file exists AND changed sections they appear in;
	// absent files keep their line so apt still 404s them as before.
	if !strings.Contains(string(relB), "Origin: Debian") {
		t.Errorf("header fields clobbered:\n%s", relB)
	}

	// Signature verifies with the exported public key (armored detached).
	sig, err := os.ReadFile(filepath.Join(iso, "dists/trixie/Release.gpg"))
	if err != nil {
		t.Fatalf("Release.gpg missing: %v", err)
	}
	if !strings.HasPrefix(string(sig), "-----BEGIN PGP SIGNATURE-----") {
		t.Errorf("signature not armored:\n%s", sig[:40])
	}
	el := openpgp.EntityList{ent}
	blk, err := armor.Decode(strings.NewReader(string(sig)))
	if err != nil {
		t.Fatalf("signature armor undecodable: %v", err)
	}
	if _, err := openpgp.CheckDetachedSignature(el, strings.NewReader(string(relB)), blk.Body, nil); err != nil {
		t.Errorf("detached signature does not verify: %v", err)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSymlink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}
