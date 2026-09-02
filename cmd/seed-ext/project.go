// cmd/seed-ext/project.go — pure projection from a SeedAsset row to
// a core.Asset, with dictionary validation. No I/O. Per PRMT-164 §4.
package main

import (
	"fmt"

	"github.com/yurimeng/cios/core"
)

// SeedDoc is the on-disk shape of seed/assets.yaml.
type SeedDoc struct {
	Assets []SeedAsset `yaml:"assets"`
}

// SeedAsset is one row of seed/assets.yaml. Attributes are copied
// verbatim into the resulting Asset.Spec (PRMT-164 §4).
type SeedAsset struct {
	Path       string         `yaml:"path"`
	Type       string         `yaml:"type"`
	Attributes map[string]any `yaml:"attributes"`
}

// knownTypes is the set loaded from protocol/types.yaml (top-level
// type keys). Project rejects any SeedAsset whose Type is absent.
//
// Project converts one SeedAsset to a core.Asset:
//   - Asset.Path = sa.Path
//   - Asset.Spec = {"type": sa.Type, ...sa.Attributes}   (attributes copied verbatim; must not contain key "type")
//   - Asset.ResourceVersion = 0  (PutAsset expectVersion=0 → create)
//
// Returns an error if Path=="" , Type not in knownTypes, or Attributes contains "type".
func Project(sa SeedAsset, knownTypes map[string]struct{}) (core.Asset, error) {
	if sa.Path == "" {
		return core.Asset{}, fmt.Errorf("seed: empty path")
	}
	if sa.Type == "" {
		return core.Asset{}, fmt.Errorf("seed: %s: empty type", sa.Path)
	}
	if _, ok := knownTypes[sa.Type]; !ok {
		return core.Asset{}, fmt.Errorf("seed: %s: type %q not in protocol/types.yaml", sa.Path, sa.Type)
	}
	if _, conflict := sa.Attributes["type"]; conflict {
		return core.Asset{}, fmt.Errorf("seed: %s: attributes must not contain key %q (reserved)", sa.Path, "type")
	}
	spec := make(map[string]any, len(sa.Attributes)+1)
	spec["type"] = sa.Type
	for k, v := range sa.Attributes {
		spec[k] = v
	}
	return core.Asset{
		Path:            sa.Path,
		Spec:            spec,
		ResourceVersion: 0,
	}, nil
}
