package testdata

import "github.com/yurimeng/cios/pkg/cpath"

// LoadDict returns a minimal cpath.Dict that contains the asset
// types and quantities referenced by the alarm rule test fixtures.
// It does NOT parse protocol YAML on disk — the fixture dict is
// constructed in-process so the rule tests stay self-contained
// without depending on the spec checker's protocol/ tree.
func LoadDict() *cpath.Dict {
	return &cpath.Dict{
		Types: map[string]cpath.TypeDef{
			"cdu":  {Parents: []string{"pod"}, Level: cpath.LevelDevice},
			"site": {},
		},
		Quantities: map[string]cpath.QuantityDef{
			"temp":     {Unit: "C"},
			"flow":     {Unit: "lpm"},
			"deltat":   {Unit: "C"},
			"leak":     {Unit: "enum", Enum: map[int]string{0: "ok", 1: "wet"}},
			"status":   {Unit: "enum", Enum: map[int]string{0: "ok", 3: "fault", 5: "offline"}},
			"pressure": {Unit: "kpa"},
			"fanrpm":   {Unit: "rpm"},
		},
	}
}
