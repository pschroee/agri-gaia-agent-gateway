// SPDX-FileCopyrightText: 2026 Philipp Schröer
//
// SPDX-License-Identifier: MIT

package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
)

// LoadEffectiveCatalog loads the model catalog the orchestrator runs with: the static file (path), the
// provider built from the environment (EnvProvider) and the default model from AGW_MODEL. AGW_DEFAULT_MODEL
// is the older name of AGW_MODEL and still read; naming both with different values is an error. Every
// check that can fail on a mistyped setting fails here, at start.
func LoadEffectiveCatalog(path string, getenv func(string) string) (*Catalog, error) {
	cat, err := LoadCatalog(path)
	if err != nil {
		return nil, err
	}
	prov, agwModel, extraHost, err := EnvProvider(getenv)
	if err != nil {
		return nil, err
	}
	if prov != nil {
		if _, exists := cat.Provider(prov.ID); exists {
			return nil, modelErr(VarProvider, "", "the provider "+prov.ID+" is already in the catalog file; choose another ID")
		}
		cat.Providers = append(cat.Providers, *prov)
		cat.ExtraHost = extraHost
	}
	legacy := strings.TrimSpace(getenv("AGW_DEFAULT_MODEL"))
	if agwModel != "" && legacy != "" && agwModel != legacy {
		return nil, errors.New("model configuration: AGW_MODEL and AGW_DEFAULT_MODEL name different models; use AGW_MODEL only")
	}
	if agwModel == "" {
		agwModel = legacy
	}
	if agwModel != "" {
		if _, _, ok := cat.Lookup(agwModel); !ok {
			return nil, fmt.Errorf("model configuration: AGW_MODEL: %q is not in the effective catalog", agwModel)
		}
		cat.Default = agwModel
	}
	return cat, nil
}

// EffectiveJSON is the canonical form of the effective catalog (file, environment provider, default model,
// host mapping): keys sorted, no key values (a provider names only the variable of its key). Its SHA-256 is
// the catalog_sha256 of the environment report and the lock, so switching the model in the .env changes it.
func (c *Catalog) EffectiveJSON() ([]byte, error) {
	b, err := json.Marshal(struct {
		Catalog   *Catalog `json:"catalog"`
		ExtraHost string   `json:"extra_host"`
	}{c, c.ExtraHost})
	if err != nil {
		return nil, err
	}
	// A round trip through a map sorts every key, whatever order the structs and fragments had.
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return nil, err
	}
	return json.Marshal(v)
}

// EffectiveSHA256 is the hex SHA-256 of EffectiveJSON.
func (c *Catalog) EffectiveSHA256() (string, error) {
	b, err := c.EffectiveJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

// mergePiExtra marshals a pi model and overlays the extra pi fields; a field named twice is an error.
func mergePiExtra(model any, extra json.RawMessage) (json.RawMessage, error) {
	b, err := json.Marshal(model)
	if err != nil || len(extra) == 0 {
		return b, err
	}
	var base, over map[string]json.RawMessage
	if err := json.Unmarshal(b, &base); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(extra, &over); err != nil {
		return nil, err
	}
	for k, v := range over {
		if _, dup := base[k]; dup {
			return nil, fmt.Errorf("pi field %s set twice", k)
		}
		base[k] = v
	}
	return json.Marshal(base)
}
