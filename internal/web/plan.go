package web

import (
	"html/template"
	"slices"
	"time"

	"github.com/alexberardi/jarvis-loop/internal/pipeline"
	"github.com/alexberardi/jarvis-loop/internal/store"
)

// The plan pages (docs/phases/07-planning.md, "The board"): a plan's brief, blueprint and Gemini's
// critique rendered through markdown's allow-list, older versions linked, its turns and the items
// it filed.

var docKinds = []string{pipeline.DocBrief, pipeline.DocBlueprint, pipeline.DocCritique}

var docNames = map[string]string{pipeline.DocBrief: "Brief", pipeline.DocBlueprint: "Blueprint", pipeline.DocCritique: "Gemini critique"}

func turnLabel(turn string) string {
	switch turn {
	case "you":
		return "your turn"
	case "agent":
		return "agent's turn"
	}
	return ""
}

// approvedVersion is the plan's approved version of a kind of document (0: none; a critique is
// never approved).
func approvedVersion(p pipeline.Plan, kind string) int {
	switch kind {
	case pipeline.DocBrief:
		return p.BriefApproved
	case pipeline.DocBlueprint:
		return p.BlueprintApproved
	}
	return 0
}

type docSection struct {
	Kind     string
	Name     string
	Doc      pipeline.PlanDoc
	HTML     template.HTML
	Approved int
	// Versions are every version, newest first, for the links.
	Versions []int
}

func section(p pipeline.Plan, docs []pipeline.PlanDoc, doc pipeline.PlanDoc) docSection {
	s := docSection{Kind: doc.Kind, Name: docNames[doc.Kind], Doc: doc, HTML: markdown(doc.Markdown), Approved: approvedVersion(p, doc.Kind)}
	for _, d := range docs {
		if d.Kind == doc.Kind {
			s.Versions = append(s.Versions, d.Version)
		}
	}
	slices.Reverse(s.Versions)
	return s
}

type planData struct {
	Plan      pipeline.Plan
	Turn      string
	RepoNames []string
	SlackURL  string
	Age       int64
	Idle      int64
	Docs      []docSection
	Turns     []pipeline.PlanTurn
	Items     []store.PlanItem
}

func planView(d store.PlanDetail, latest map[string]pipeline.PlanDoc, now time.Time, slackBase string) planData {
	p := d.Plan
	v := planData{Plan: p, Turn: d.Turn, RepoNames: repoNames(p.Repos), SlackURL: slackURL(slackBase, p.SlackChannel, p.SlackTS),
		Age: int64(now.Sub(p.CreatedAt) / time.Second), Idle: int64(now.Sub(p.UpdatedAt) / time.Second), Turns: d.Turns, Items: d.Items}
	for _, kind := range docKinds {
		if doc, ok := latest[kind]; ok {
			v.Docs = append(v.Docs, section(p, d.Docs, doc))
		}
	}
	return v
}

type docData struct {
	Plan pipeline.Plan
	docSection
	// Latest is the newest version of the kind.
	Latest int
}

func docView(d store.PlanDetail, doc pipeline.PlanDoc) docData {
	s := section(d.Plan, d.Docs, doc)
	return docData{Plan: d.Plan, docSection: s, Latest: s.Versions[0]}
}
