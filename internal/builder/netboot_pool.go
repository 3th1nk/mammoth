package builder

import (
	"crypto/md5"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/ProtonMail/go-crypto/openpgp"
)

// StageNetbootPool turns the unpacked-ISO subtree into a complete, signed
// offline mirror for the d-i netboot carrier (real-hardware proven on a
// 2288H):
//
//  1. udeb fill (optional): the netinst ISO prunes the netboot-only installer
//     components (kernel-image-*-di above all — its index lists ~230 udebs
//     with no kernel image at all), so anna dies with "No kernel modules
//     were found". udebsDir is a deployment-staged archive subset
//     (scripts/fetch-di-udebs.sh shape: pool/ + dists/<suite>/main/
//     debian-installer/binary-amd64/Packages[.gz]); its missing pool files
//     are copied in and its complete index replaces the pruned one.
//  2. Release checksum rewrite: any replaced index invalidates the ISO's own
//     Release checksums — every checksum section (MD5Sum/SHA1/SHA256/SHA512)
//     must agree again or anna loops on re-downloading the index forever.
//  3. Release.gpg: the pool is served over HTTP, and apt-setup's mirror
//     verification runs a signature-checking apt-get update that
//     allow_unauthenticated does not cover. The signature is armored —
//     current apt rejects binary detached signatures.
//  4. by-hash backfill: the component Releases advertise Acquire-By-Hash:
//     yes but the netinst ISO ships no by-hash store at all, so apt-setup's
//     verification 404s on dists/<suite>/<comp>/binary-*/by-hash/SHA256/<sha>
//     and "Configure the package manager" reports "failed to access the
//     mirror" even though the signature chain is sound.
//
// The suite is discovered from the tree (the single non-symlink dists/
// entry); the ISO's symlink aliases (stable → trixie) keep working.
func StageNetbootPool(isoDir, udebsDir string, ent *openpgp.Entity) error {
	suite, err := poolSuite(isoDir)
	if err != nil {
		return err
	}
	if udebsDir != "" {
		if err := fillUdebs(isoDir, udebsDir, suite); err != nil {
			return err
		}
	}
	if err := indexByHash(isoDir, suite); err != nil {
		return err
	}
	relPath := filepath.Join(isoDir, "dists", suite, "Release")
	rel, err := os.ReadFile(relPath)
	if err != nil {
		return fmt.Errorf("builder: pool Release missing: %w", err)
	}
	rewritten, err := rewriteReleaseChecksums(string(rel), isoDir, suite)
	if err != nil {
		return err
	}
	if err := os.WriteFile(relPath, []byte(rewritten), 0o644); err != nil {
		return err
	}
	return SignReleaseDetached(ent, []byte(rewritten), relPath+".gpg")
}

// poolSuite finds the suite directory under dists/ — the ISO layout has one
// real suite plus optional symlink aliases (stable → trixie).
func poolSuite(isoDir string) (string, error) {
	entries, err := os.ReadDir(filepath.Join(isoDir, "dists"))
	if err != nil {
		return "", fmt.Errorf("builder: pool dists/ missing: %w", err)
	}
	var suites []string
	for _, e := range entries {
		if e.IsDir() && e.Type()&fs.ModeSymlink == 0 {
			suites = append(suites, e.Name())
		}
	}
	if len(suites) != 1 {
		return "", fmt.Errorf("builder: expected exactly one suite dir under dists/, got %v", suites)
	}
	return suites[0], nil
}

// fillUdebs copies the staged pool files the tree is missing and replaces
// the pruned installer index with the staged complete one.
func fillUdebs(isoDir, udebsDir, suite string) error {
	// pool/ subtree: only missing files (same relative layout).
	err := filepath.WalkDir(filepath.Join(udebsDir, "pool"), func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		rel, rerr := filepath.Rel(udebsDir, path)
		if rerr != nil {
			return rerr
		}
		dst := filepath.Join(isoDir, rel)
		if _, serr := os.Stat(dst); serr == nil {
			return nil
		}
		if merr := os.MkdirAll(filepath.Dir(dst), 0o755); merr != nil {
			return merr
		}
		return copyFile(path, dst)
	})
	if err != nil {
		return fmt.Errorf("builder: udeb fill failed: %w", err)
	}
	// The complete installer index replaces the ISO's pruned one.
	idx := filepath.Join("dists", suite, "main", "debian-installer", "binary-amd64")
	for _, name := range []string{"Packages", "Packages.gz"} {
		src := filepath.Join(udebsDir, idx, name)
		if _, serr := os.Stat(src); serr != nil {
			return fmt.Errorf("builder: staged index missing: %s (re-run fetch-di-udebs.sh)", src)
		}
		dst := filepath.Join(isoDir, idx, name)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		if err := copyFile(src, dst); err != nil {
			return err
		}
	}
	return nil
}

// indexByHash backfills the by-hash store for every component index under
// dists/<suite> (both binary-amd64 and binary-amd64/debian-installer
// shapes). Idempotent: existing entries are kept, so re-staging a shared
// content-addressed pool never rewrites served history.
func indexByHash(isoDir, suite string) error {
	root := filepath.Join(isoDir, "dists", suite)
	return filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		switch filepath.Base(path) {
		case "Packages", "Packages.gz", "Packages.xz":
		default:
			return nil
		}
		h := sha256.New()
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		dst := filepath.Join(filepath.Dir(path), "by-hash", "SHA256", hex.EncodeToString(h.Sum(nil)))
		if _, err := os.Stat(dst); err == nil {
			return nil
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return copyFile(path, dst)
	})
}

// releaseChecksumSections are the checksum blocks a Release carries, in the
// digest each block records. SHA1 is on the list deliberately: the ISO's
// Release has that section and a stale SHA1 entry keeps anna looping.
var releaseChecksumSections = []struct {
	name   string
	digest func([]byte) string
}{
	{"MD5Sum", func(b []byte) string { h := md5.Sum(b); return hex.EncodeToString(h[:]) }},
	{"SHA1", func(b []byte) string { h := sha1.Sum(b); return hex.EncodeToString(h[:]) }},
	{"SHA256", func(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }},
	{"SHA512", func(b []byte) string { h := sha512.Sum512(b); return hex.EncodeToString(h[:]) }},
}

var releaseEntryRe = regexp.MustCompile(`^ (\S+) +(\d+) +(\S+)$`)

// rewriteReleaseChecksums recomputes the checksum entries for every index
// file that exists in the tree (the pool is self-describing: whatever the
// Release lists and the tree carries must agree byte-for-byte).
func rewriteReleaseChecksums(release, isoDir, suite string) (string, error) {
	lines := strings.Split(release, "\n")
	section := ""
	for i, line := range lines {
		if !strings.HasSuffix(line, ":") && line != "" && !strings.HasPrefix(line, " ") {
			section = "" // any field line ends the current checksum block
			continue
		}
		if m := regexp.MustCompile(`^(MD5Sum|SHA1|SHA256|SHA512):$`).FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			section = m[1]
			continue
		}
		if section == "" {
			continue
		}
		m := releaseEntryRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		rel := m[3]
		path := filepath.Join(isoDir, "dists", suite, rel)
		data, err := os.ReadFile(path)
		if err != nil {
			continue // file absent from the tree: entry untouched (apt 404s it as before)
		}
		for _, s := range releaseChecksumSections {
			if s.name != section {
				continue
			}
			lines[i] = fmt.Sprintf(" %s %d %s", s.digest(data), len(data), rel)
		}
	}
	return strings.Join(lines, "\n"), nil
}

func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, in)
	return err
}
