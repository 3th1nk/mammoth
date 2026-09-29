package sandbox

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// EnsureISO returns the local path of the pinned alpine extended ISO,
// downloading it on first use (resumable, with a progress bar) and verifying
// it against the upstream sha256. The cache lives in the sandbox dir and
// survives `sandbox down`.
func EnsureISO(dir string) (string, error) {
	iso := filepath.Join(dir, alpineISO)
	sumFile := iso + ".sha256"

	if _, err := os.Stat(iso); err == nil {
		want, err := cachedChecksum(sumFile)
		switch {
		case err == nil && fileChecksum(iso) == want:
			fmt.Printf("  ✓ using cached %s\n", alpineISO)
			return iso, nil // verified cache
		case err == nil:
			fmt.Printf("  cached %s failed checksum — re-downloading\n", alpineISO)
			_ = os.Remove(iso)
		default:
			// no cached checksum: verify once, then trust the cache
			if err := fetchChecksum(sumFile); err != nil {
				return "", fmt.Errorf("fetch %s checksum: %w", alpineISO, err)
			}
			want, err := cachedChecksum(sumFile)
			if err != nil {
				return "", err
			}
			if fileChecksum(iso) != want {
				return "", fmt.Errorf("cached %s does not match upstream sha256 — remove %s and retry", alpineISO, iso)
			}
			return iso, nil
		}
	}

	if err := fetchChecksum(sumFile); err != nil {
		return "", fmt.Errorf("fetch %s checksum: %w", alpineISO, err)
	}
	url := alpineISOURL()
	fmt.Printf("· downloading %s (about 1 GB, resumable)\n", alpineISO)
	cmd := exec.Command("curl", "-fL", "-C", "-", "--progress-bar", "-o", iso, url)
	cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return "", fmt.Errorf("download %s: %w (rerun to resume)", url, err)
	}
	if want, err := cachedChecksum(sumFile); err != nil || fileChecksum(iso) != want {
		return "", fmt.Errorf("%s failed checksum verification", alpineISO)
	}
	return iso, nil
}

func fetchChecksum(dest string) error {
	cmd := exec.Command("curl", "-fsSL", "-o", dest, alpineISOURL()+".sha256")
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("%s", firstLine(out))
	}
	return nil
}

// cachedChecksum parses the alpine checksum file shape: "<hex>  <filename>".
func cachedChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 2 && strings.HasSuffix(fields[1], alpineISO) {
			return fields[0], nil
		}
	}
	return "", fmt.Errorf("no %s entry in %s", alpineISO, path)
}

func fileChecksum(path string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, bufio.NewReader(f)); err != nil {
		return ""
	}
	return hex.EncodeToString(h.Sum(nil))
}
