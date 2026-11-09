package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Project is one entry in the host-side registry.
type Project struct {
	Path    string            `toml:"path"`
	Type    string            `toml:"type"`
	Image   string            `toml:"image"`
	Agents  []string          `toml:"agents"`         // first one is the default
	Mode    string            `toml:"mode,omitempty"` // permission mode: ask | auto | bypass (default auto)
	CPUs    int               `toml:"cpus,omitempty"`
	Memory  string            `toml:"memory,omitempty"`
	Disk    string            `toml:"disk,omitempty"`
	Secrets map[string]string `toml:"secrets,omitempty"` // env var -> host file holding the value
}

type Defaults struct {
	Type   string   `toml:"type"`
	Image  string   `toml:"image"`
	Agents []string `toml:"agents,omitempty"` // empty: ABOX_DEFAULT_AGENTS from config.env
	Mode   string   `toml:"mode,omitempty"`   // empty: auto
}

// Registry lives on the host (never inside a sandbox) so a sandbox cannot
// change its own settings.
type Registry struct {
	Defaults Defaults            `toml:"defaults"`
	Projects map[string]*Project `toml:"projects"`
}

func registryPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, _ := os.UserHomeDir()
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "abox", "projects.toml")
}

func loadRegistry() (*Registry, error) {
	r := &Registry{Projects: map[string]*Project{}}
	p := registryPath()
	if _, err := os.Stat(p); err == nil {
		md, err := toml.DecodeFile(p, r)
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}
		if und := md.Undecoded(); len(und) > 0 {
			keys := make([]string, len(und))
			for i, k := range und {
				keys[i] = k.String()
			}
			return nil, fmt.Errorf("%s: unknown keys: %s", p, strings.Join(keys, ", "))
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if r.Projects == nil {
		r.Projects = map[string]*Project{}
	}
	if r.Defaults.Type == "" {
		r.Defaults.Type = "container"
	}
	if r.Defaults.Image == "" {
		r.Defaults.Image = "ubuntu"
	}
	if r.Defaults.Mode != "" && !validMode(r.Defaults.Mode) {
		return nil, fmt.Errorf("%s: [defaults] mode %q must be one of %s", p, r.Defaults.Mode, strings.Join(permModes, ", "))
	}
	for name, pr := range r.Projects {
		if pr == nil || pr.Path == "" {
			return nil, fmt.Errorf("%s: project %q has no path", p, name)
		}
		if pr.Mode != "" && !validMode(pr.Mode) {
			return nil, fmt.Errorf("%s: project %q: mode %q must be one of %s", p, name, pr.Mode, strings.Join(permModes, ", "))
		}
		if pr.Type == "" {
			pr.Type = r.Defaults.Type
		}
		if pr.Image == "" {
			pr.Image = r.Defaults.Image
		}
	}
	return r, nil
}

func (r *Registry) Save() error {
	p := registryPath()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	var buf bytes.Buffer
	buf.WriteString("# abox project registry (edit with `abox edit`; `abox doctor` validates)\n\n")
	if err := toml.NewEncoder(&buf).Encode(r); err != nil {
		return err
	}
	tmp := p + ".tmp"
	if err := os.WriteFile(tmp, buf.Bytes(), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, p)
}

func (r *Registry) Names() []string {
	names := make([]string, 0, len(r.Projects))
	for n := range r.Projects {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
