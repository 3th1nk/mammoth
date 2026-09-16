package builder

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"io"
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
	// The main binary index is real gzip — appendIndexStanza gunzips it.
	mustWrite(t, filepath.Join(iso, "dists/trixie/main/debian-installer/binary-amd64/Packages"), "pruned-index")
	mustWrite(t, filepath.Join(iso, "dists/trixie/main/debian-installer/binary-amd64/Packages.gz"), "pruned-index-gz")
	mustWriteBytes(t, filepath.Join(iso, "dists/trixie/main/binary-amd64/Packages.gz"), gzBytes(t, "deb-index\n"))
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

	// by-hash backfilled for every component index — the replaced installer
	// one and the untouched deb one alike (the ISO advertises by-hash but
	// ships no store; apt-setup's mirror verify 404s without this). Each
	// by-hash entry must byte-match its index file.
	for _, name := range []string{
		"main/debian-installer/binary-amd64/Packages.gz",
		"main/binary-amd64/Packages.gz",
	} {
		idxPath := filepath.Join(iso, "dists/trixie", name)
		sum := sha256.Sum256(mustRead(t, idxPath))
		bh := filepath.Join(filepath.Dir(idxPath), "by-hash", "SHA256", hex.EncodeToString(sum[:]))
		got, err := os.ReadFile(bh)
		if err != nil {
			t.Errorf("by-hash entry missing for %s: %v", name, err)
		} else if !bytes.Equal(got, mustRead(t, idxPath)) {
			t.Errorf("by-hash entry wrong for %s", name)
		}
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

	// The key-delivery deb: a parseable ar carrying the keyring at
	// /etc/apt/trusted.gpg.d/mammoth-pool.gpg, registered in the main index
	// with a stanza whose digests match the deb bytes (debootstrap checks).
	deb, err := os.ReadFile(filepath.Join(iso, "pool/main/m/mammoth-key/mammoth-key_1.0_all.deb"))
	if err != nil {
		t.Fatalf("key deb missing: %v", err)
	}
	if dir := os.Getenv("MAMMOTH_TEST_ARTIFACT_DIR"); dir != "" {
		mustWriteBytes(t, filepath.Join(dir, "mammoth-key_1.0_all.deb"), deb)
	}
	files, ok := dataFromDeb(t, deb)
	if !ok {
		t.Fatalf("key deb unreadable")
	}
	keyring := files["etc/apt/trusted.gpg.d/mammoth-pool.gpg"]
	if keyring == nil {
		t.Fatalf("key deb carries no keyring")
	}
	wantKey, _ := PoolPublicKey(ent)
	if !bytes.Equal(keyring, wantKey) {
		t.Errorf("key deb carries wrong keyring bytes")
	}
	if len(files["etc/apt/apt.conf.d/99mammoth-offline"]) == 0 {
		t.Errorf("key deb missing the offline-apt tolerances")
	}
	idx := gunzipBytes(t, mustRead(t, filepath.Join(iso, "dists/trixie/main/binary-amd64/Packages.gz")))
	sum := sha256.Sum256(deb)
	for _, want := range []string{
		"Package: mammoth-key",
		"Filename: pool/main/m/mammoth-key/mammoth-key_1.0_all.deb",
		"Size: " + strconv.Itoa(len(deb)),
		"SHA256: " + hex.EncodeToString(sum[:]),
	} {
		if !strings.Contains(idx, want) {
			t.Errorf("main index stanza missing %q", want)
		}
	}
	// Re-staging the shared pool must not duplicate the stanza.
	if err := StageNetbootPool(iso, udebs, ent); err != nil {
		t.Fatalf("restage: %v", err)
	}
	idx = gunzipBytes(t, mustRead(t, filepath.Join(iso, "dists/trixie/main/binary-amd64/Packages.gz")))
	if strings.Count(idx, "Package: mammoth-key\n") != 1 {
		t.Errorf("restage duplicated the key stanza:\n%s", idx)
	}
}

// dataFromDeb walks the ar container (debian-binary, control.tar.gz,
// data.tar.gz) and returns every data.tar.gz entry keyed by path.
func dataFromDeb(t *testing.T, deb []byte) (map[string][]byte, bool) {
	t.Helper()
	out := map[string][]byte{}
	if string(deb[:8]) != "!<arch>\n" {
		return out, false
	}
	off := 8
	for off+60 <= len(deb) {
		name := strings.TrimRight(string(deb[off:off+16]), " ")
		size, err := strconv.Atoi(strings.TrimSpace(string(deb[off+48 : off+58])))
		if err != nil {
			return out, false
		}
		data := deb[off+60 : off+60+size]
		off += 60 + size + size%2
		if name != "data.tar.gz/" {
			continue
		}
		tr := tar.NewReader(bytes.NewReader(gunzipRaw(t, data)))
		for {
			hdr, err := tr.Next()
			if err == io.EOF {
				return out, true
			}
			if err != nil {
				return out, false
			}
			b, err := io.ReadAll(tr)
			if err != nil {
				return out, false
			}
			out[hdr.Name] = b
		}
	}
	return out, false
}

func gunzipRaw(t *testing.T, b []byte) []byte {
	t.Helper()
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		t.Fatalf("gunzip: %v", err)
	}
	out, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip read: %v", err)
	}
	return out
}

func gunzipBytes(t *testing.T, b []byte) string {
	return string(gunzipRaw(t, b))
}

func mustRead(t *testing.T, path string) []byte {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return b
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	mustWriteBytes(t, path, []byte(content))
}

func mustWriteBytes(t *testing.T, path string, content []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, content, 0o644); err != nil {
		t.Fatal(err)
	}
}

func gzBytes(t *testing.T, s string) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write([]byte(s)); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func mustSymlink(t *testing.T, path, target string) {
	t.Helper()
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
}
