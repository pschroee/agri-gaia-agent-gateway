// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package exp

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// BaseFile is the name of the file next to plan.yaml that lists the base datasets (BM1).
const BaseFile = "grundbestand.yaml"

// BaseRef is the prefix of an identifier in a rule that names a base dataset: "base:<name>". The runner
// replaces it by the dataset's identifier from grundbestand.yaml.
const BaseRef = "base:"

// BaseSet is the content of grundbestand.yaml: datasets that were created once, outside the runs, and are
// only delegated for reading. The runner checks them before every run that uses them.
type BaseSet struct {
	Owner    string        `yaml:"owner"`
	Datasets []BaseDataset `yaml:"datasets"`
}

// BaseDataset describes one base dataset as it must look on the platform.
type BaseDataset struct {
	Name          string `yaml:"name"`
	ID            int64  `yaml:"id"`
	Filecount     int    `yaml:"filecount"`
	TotalFilesize int64  `yaml:"total_filesize"`
	FilesSHA256   string `yaml:"files_sha256"`
}

// LoadBaseSet reads grundbestand.yaml from dir. A missing file is no error: it returns nil, so that
// material without base datasets validates as before.
func LoadBaseSet(dir string) (*BaseSet, error) {
	path := filepath.Join(dir, BaseFile)
	if _, err := os.Stat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	b, err := loadYAML[BaseSet](path)
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	for i, d := range b.Datasets {
		switch {
		case strings.TrimSpace(d.Name) == "":
			return nil, fmt.Errorf("%s: datasets[%d]: name is empty", path, i)
		case d.ID <= 0:
			return nil, fmt.Errorf("%s: dataset %q: id must be positive", path, d.Name)
		case seen[d.Name]:
			return nil, fmt.Errorf("%s: dataset %q is listed twice", path, d.Name)
		}
		seen[d.Name] = true
	}
	return b, nil
}

// Lookup returns the base dataset of that name. It is safe on a nil set.
func (b *BaseSet) Lookup(name string) (BaseDataset, bool) {
	if b == nil {
		return BaseDataset{}, false
	}
	for _, d := range b.Datasets {
		if d.Name == name {
			return d, true
		}
	}
	return BaseDataset{}, false
}

// IDs maps every name to its identifier as text, the form ResolveFixtureRefs substitutes.
func (b *BaseSet) IDs() map[string]string {
	out := map[string]string{}
	if b == nil {
		return out
	}
	for _, d := range b.Datasets {
		out[d.Name] = strconv.FormatInt(d.ID, 10)
	}
	return out
}

// CompareBase compares the expected description with the platform's answer for GET /datasets/{id}
// (the object may carry the metadata fields at top level or below "metadata"). It returns one finding
// per deviating field, "<field> <expected> → <actual>"; an empty result means unchanged. A field the
// answer does not carry counts as a deviation.
func CompareBase(want BaseDataset, got map[string]any) []string {
	var out []string
	lookup := func(key string) (any, bool) {
		if v, ok := got[key]; ok {
			return v, true
		}
		if md, ok := got["metadata"].(map[string]any); ok {
			v, ok := md[key]
			return v, ok
		}
		return nil, false
	}
	if v, ok := got["name"]; !ok {
		out = append(out, fmt.Sprintf("name %s → missing", want.Name))
	} else if s, _ := v.(string); s != want.Name {
		out = append(out, fmt.Sprintf("name %s → %v", want.Name, v))
	}
	if v, ok := lookup("filecount"); !ok {
		out = append(out, fmt.Sprintf("filecount %d → missing", want.Filecount))
	} else if n, ok := intValue(v); !ok || n != want.Filecount {
		out = append(out, fmt.Sprintf("filecount %d → %v", want.Filecount, v))
	}
	if v, ok := lookup("total_filesize"); !ok {
		out = append(out, fmt.Sprintf("total_filesize %d → missing", want.TotalFilesize))
	} else if n, ok := int64Value(v); !ok || n != want.TotalFilesize {
		out = append(out, fmt.Sprintf("total_filesize %d → %v", want.TotalFilesize, v))
	}
	return out
}

// int64Value reads a whole number as JSON or YAML gives it.
func int64Value(v any) (int64, bool) {
	switch n := v.(type) {
	case int:
		return int64(n), true
	case int64:
		return n, true
	case uint64:
		return int64(n), true
	case float64:
		if n == float64(int64(n)) {
			return int64(n), true
		}
	}
	return 0, false
}
