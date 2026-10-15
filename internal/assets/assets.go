// Package assets holds files embedded into the binary.
package assets

import _ "embed"

// Defaults holds the built-in settings and profiles. `so init` writes it out as
// the starter config.
//
//go:embed defaults.toml
var Defaults string

// Modes holds the built-in modes. They are always loaded, so they update with
// the binary, and a user config overrides them field by field.
//
//go:embed modes.toml
var Modes string

// Skill is the agent skill that teaches a harness how to call `so`.
//
//go:embed SKILL.md
var Skill string
