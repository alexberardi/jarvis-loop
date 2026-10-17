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

// Product is the planning stage's product persona (skills/product/SKILL.md).
//
//go:embed product/SKILL.md
var Product string

// Engineering is the planning stage's engineering persona (skills/engineering/SKILL.md).
//
//go:embed engineering/SKILL.md
var Engineering string

// Orchestrator is the orchestrator's persona (skills/orchestrator/SKILL.md), the start of each
// day's orchestrate session.
//
//go:embed orchestrator/SKILL.md
var Orchestrator string
