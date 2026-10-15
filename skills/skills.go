// Package skills embeds the stage skills, the briefs loop agents are given.
package skills

import _ "embed"

// Build is the build stage's skill (skills/build/SKILL.md).
//
//go:embed build/SKILL.md
var Build string

// Triage is the triage stage's skill (skills/triage/SKILL.md).
//
//go:embed triage/SKILL.md
var Triage string

// Review is the review stage's skill (skills/review/SKILL.md).
//
//go:embed review/SKILL.md
var Review string
