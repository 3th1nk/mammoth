// Package compat loads the vendor compatibility matrix (docs/compat/) and
// answers capability questions with graceful degradation instead of failure:
// known blind spots annotate inventory coverage; missing one-shot boot or a
// single media slot shapes install-time compensation and media strategy as
// those features land (docs/07-bmc.md §4).
package compat

import (
	"embed"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"gopkg.in/yaml.v3"
)

//go:embed matrix
var embeddedFS embed.FS

// Entry is one vendor's declared behaviors. Absent booleans mean "unknown"
// and never trigger degradation by themselves.
type Entry struct {
	Vendor             string   `yaml:"vendor"`
	Models             []string `yaml:"models"` // regexes against Redfish Model; empty = all models
	PartialInventory   []string `yaml:"partial_inventory"`
	SingleVirtualMedia bool     `yaml:"single_virtual_media"`
	RemoteURIMount     *bool    `yaml:"remote_uri_mount"`
	OneShotBoot        *bool    `yaml:"one_shot_boot"`
	KnownDefects       []Defect `yaml:"known_defects"`

	modelRes []*regexp.Regexp
}

type Defect struct {
	Firmware   string `yaml:"firmware"` // expression like ">=1.0 <1.23"
	Issue      string `yaml:"issue"`
	Workaround string `yaml:"workaround"`
}

// Registry holds the loaded matrix.
type Registry struct {
	entries []*Entry
}

// Default returns the matrix embedded at build time.
func Default() (*Registry, error) {
	return loadFS(embeddedFS)
}

// LoadDir reads *.yaml entries from dir on top of the embedded defaults.
// A broken or missing override directory degrades to defaults with a warning
// returned as error for the caller to log.
func LoadDir(dir string) (*Registry, error) {
	r, err := Default()
	if err != nil {
		return nil, err
	}
	matches, err := filepath.Glob(filepath.Join(dir, "*.yaml"))
	if err != nil {
		return r, nil
	}
	for _, m := range matches {
		e, err := parseFile(m)
		if err != nil {
			return nil, fmt.Errorf("compat: %s: %w", m, err)
		}
		r.entries = append(r.entries, e)
	}
	return r, nil
}

func loadFS(fsys embed.FS) (*Registry, error) {
	r := &Registry{}
	matches, err := fs.Glob(fsys, "matrix/*.yaml")
	if err != nil {
		return nil, err
	}
	for _, m := range matches {
		b, err := fsys.ReadFile(m)
		if err != nil {
			return nil, err
		}
		e, err := parse(b)
		if err != nil {
			return nil, fmt.Errorf("compat: %s: %w", m, err)
		}
		r.entries = append(r.entries, e)
	}
	return r, nil
}

func parseFile(path string) (*Entry, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return parse(b)
}

func parse(b []byte) (*Entry, error) {
	var e Entry
	if err := yaml.Unmarshal(b, &e); err != nil {
		return nil, err
	}
	if e.Vendor == "" {
		return nil, fmt.Errorf("entry without vendor")
	}
	e.Vendor = normalize(e.Vendor)
	for _, pat := range e.Models {
		re, err := regexp.Compile("(?i)" + pat)
		if err != nil {
			return nil, fmt.Errorf("vendor %s: model regex %q: %w", e.Vendor, pat, err)
		}
		e.modelRes = append(e.modelRes, re)
	}
	return &e, nil
}

// Lookup finds the entry matching vendor (and model when declared).
func (r *Registry) Lookup(vendor, model string) *Entry {
	if r == nil {
		return nil
	}
	v := normalize(vendor)
	for _, e := range r.entries {
		if e.Vendor != v {
			continue
		}
		if len(e.modelRes) == 0 {
			return e
		}
		for _, re := range e.modelRes {
			if re.MatchString(model) {
				return e
			}
		}
	}
	return nil
}

// InventoryNotes returns the declared spec-level blind spots for a machine,
// consumed by the discovery flow to annotate coverage: partial.
func (r *Registry) InventoryNotes(vendor, model string) []string {
	e := r.Lookup(vendor, model)
	if e == nil {
		return nil
	}
	return e.PartialInventory
}

// OneShotBoot reports the declared one-shot boot support; ok=false means the
// matrix does not declare it (callers must not assume).
func (r *Registry) OneShotBoot(vendor, model string) (supported, declared bool) {
	e := r.Lookup(vendor, model)
	if e == nil || e.OneShotBoot == nil {
		return true, false
	}
	return *e.OneShotBoot, true
}

func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
