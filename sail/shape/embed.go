package shape

import (
	_ "embed"
	"encoding/json"
)

// tuned holds the parameters and the ordinary-HTTPS profile that
// `sailtrace tune` produced, measured against the classifier in this package.
// Both ends of a tunnel must agree on them, so they ship in the binary rather
// than in a configuration file; SAIL_SHAPE overrides for experiments.
//
//go:embed tuned.json
var tuned []byte

func init() {
	var p Params
	if err := json.Unmarshal(tuned, &p); err == nil && p.Coalesce > 0 {
		Set(p)
	}
	_ = LoadEnv() // SAIL_SHAPE wins, for measurement runs
}
