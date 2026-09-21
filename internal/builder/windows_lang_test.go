package builder

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

func TestParseWindowsLangIni(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want string
	}{
		{
			name: "single language media (real zh-CN Server 2019 shape)",
			in:   "\r\n[Available UI Languages]\r\nzh-cn = 3\r\n\r\n[Fallback Languages]\r\nzh-cn = en-us\r\n",
			want: "zh-cn",
		},
		{
			name: "multi language: first listed entry is the media default",
			in:   "[Available UI Languages]\r\nen-us = 3\r\nzh-cn = 3\r\n",
			want: "en-us",
		},
		{
			name: "other sections first",
			in:   "[Languages]\r\nfoo = 1\r\n[AVAILABLE UI LANGUAGES]\r\nja-jp = 3\r\n",
			want: "ja-jp",
		},
		{
			name: "comment and blank lines skipped",
			in:   "; media languages\r\n\r\n[Available UI Languages]\r\n  de-de = 3  \r\n",
			want: "de-de",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseWindowsLangIni([]byte(tc.in))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if got != tc.want {
				t.Fatalf("parse = %q, want %q", got, tc.want)
			}
		})
	}
	for _, name := range []string{"no section", "[Available UI Languages]\r\n"} {
		if _, err := parseWindowsLangIni([]byte(name)); err == nil {
			t.Errorf("parse(%q) succeeded, want error", name)
		}
	}
}

func TestDetectWindowsMediaLanguageLayers(t *testing.T) {
	ctx := context.Background()
	winDir := func(base, sha string) string {
		return filepath.Join(base, "win", sha)
	}

	// Layer 1: the cache file wins over everything (even a broken tree).
	base := t.TempDir()
	wd := winDir(base, "s1")
	if err := os.MkdirAll(wd, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wd, "media-language"), []byte("fr-fr\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lang, err := DetectWindowsMediaLanguage(ctx, "/nonexistent.iso", "s1", wd)
	if err != nil || lang != "fr-fr" {
		t.Fatalf("cache layer = %q, %v; want fr-fr, nil", lang, err)
	}

	// Layer 2: the prepared tree's lang.ini (and the value gets cached).
	base2 := t.TempDir()
	wd2 := winDir(base2, "s2")
	tree := filepath.Join(wd2, "tree", "sources")
	if err := os.MkdirAll(tree, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(tree, "lang.ini"), []byte("[Available UI Languages]\r\nzh-cn = 3\r\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	lang, err = DetectWindowsMediaLanguage(ctx, "/nonexistent.iso", "s2", wd2)
	if err != nil || lang != "zh-cn" {
		t.Fatalf("tree layer = %q, %v; want zh-cn, nil", lang, err)
	}
	if b, rerr := os.ReadFile(filepath.Join(wd2, "media-language")); rerr != nil || string(b) != "zh-cn\n" {
		t.Fatalf("cache file after tree hit = %q, %v", b, rerr)
	}

	// Layer 3 miss: no cache, no tree, no ISO — the extraction error is the
	// return (the caller degrades to the driver default).
	base3 := t.TempDir()
	if _, err := DetectWindowsMediaLanguage(ctx, "/nonexistent.iso", "s3", winDir(base3, "s3")); err == nil {
		t.Fatal("detection with no cache/tree/iso succeeded, want error")
	}
}
