// Package skills embeds the stage skills, the briefs loop agents are given.
package skills

import _ "embed"

// Build is the build stage's skill (skills/build/SKILL.md).
//
//go:embed build/SKILL.md
var Build string
