package pipeline

import (
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"
)

// Event kinds of the user's corrections to an item (loop edit, unlink, resources). Each records
// what it replaced.
const (
	KindEdited    = "edited"
	KindUnlinked  = "unlinked"
	KindResources = "resources"
	KindFeature   = "feature"
)

// BeforeBuild reports whether the item is still in triage's hands: in triage, or on triage's
// question. Its title and ask may change only then (a build already works from them), and its
// resources only after (triage's outcome sets them).
func BeforeBuild(it Item) bool {
	return it.Stage == Triage || (it.Stage == NeedsYou && it.ReturnStage == Triage)
}

// ApplyEdit fixes an item's title or ask before build starts; the event keeps the old text.
func ApplyEdit(it Item, title, ask *string) (Transition, error) {
	if title == nil && ask == nil {
		return Transition{}, errf(CodeMissingField, "nothing to change: give title or ask")
	}
	if !BeforeBuild(it) {
		return Transition{}, errf(CodeWrongStage, "item is %s: the title and ask change only before build starts", it.Stage)
	}
	data := map[string]any{}
	if title != nil {
		t := strings.TrimSpace(*title)
		if t == "" {
			return Transition{}, errf(CodeInvalidField, "title cannot be empty")
		}
		data["title"] = map[string]string{"from": it.Title, "to": t}
		it.Title = t
	}
	if ask != nil {
		a := strings.TrimSpace(*ask)
		data["ask"] = map[string]string{"from": it.Ask, "to": a}
		it.Ask = a
	}
	return Transition{Item: it, From: it.Stage, To: it.Stage, Kind: KindEdited, Data: data}, nil
}

// ApplyUnlink stops an open item waiting for after. With no dependency left it is claimable at
// once: claims read depends_on as it stands.
func ApplyUnlink(it Item, after int64) (Transition, error) {
	if it.Stage == Done || it.Stage == Closed {
		return Transition{}, errf(CodeWrongStage, "item is %s", it.Stage)
	}
	if !slices.Contains(it.DependsOn, after) {
		return Transition{}, errf(CodeInvalidField, "#%d does not wait for #%d", it.ID, after)
	}
	it.DependsOn = slices.DeleteFunc(slices.Clone(it.DependsOn), func(d int64) bool { return d == after })
	return Transition{Item: it, From: it.Stage, To: it.Stage, Kind: KindUnlinked,
		Data: map[string]any{"after": after, "depends_on": it.DependsOn}}, nil
}

// ApplyResources replaces the resources triage set (a comma list; empty or "none" for none). It
// applies from the next claim; a run in progress keeps its runner.
func ApplyResources(it Item, raw string) (Transition, error) {
	if it.Stage == Done || it.Stage == Closed {
		return Transition{}, errf(CodeWrongStage, "item is %s", it.Stage)
	}
	if BeforeBuild(it) {
		return Transition{}, errf(CodeWrongStage, "item is in triage, which sets its resources: change them after triage")
	}
	res := []string{}
	if strings.TrimSpace(raw) != "none" {
		var err error
		if res, err = ParseResources(raw); err != nil {
			return Transition{}, err
		}
	}
	from := it.Resources
	if from == nil {
		from = []string{}
	}
	it.Resources = res
	return Transition{Item: it, From: it.Stage, To: it.Stage, Kind: KindResources,
		Data: map[string]any{"from": from, "to": res}}, nil
}

// MaxFeatureLen caps a feature name; a longer plan title is cut there.
const MaxFeatureLen = 48

// FeatureSlug normalizes a feature name: lowercase, letters and digits, each other run a '-'
// ("Smallest Blocks" is smallest-blocks). "" when nothing is left.
func FeatureSlug(name string) string {
	words := strings.FieldsFunc(strings.ToLower(name), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	s := strings.Join(words, "-")
	if len(s) > MaxFeatureLen {
		cut := MaxFeatureLen
		for !utf8.RuneStart(s[cut]) {
			cut--
		}
		s = strings.TrimRight(s[:cut], "-")
	}
	return s
}

// ParseFeature normalizes a feature name the user gave: "" or "none" is no feature, and a name
// with no letters or digits is refused.
func ParseFeature(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "none" {
		return "", nil
	}
	f := FeatureSlug(raw)
	if f == "" {
		return "", errf(CodeInvalidField, "feature %q has no letters or digits", raw)
	}
	return f, nil
}

// ApplyFeature tags the item with a feature ("" or none clears it), in any stage: a feature's
// progress counts its done items too.
func ApplyFeature(it Item, raw string) (Transition, error) {
	f, err := ParseFeature(raw)
	if err != nil {
		return Transition{}, err
	}
	data := map[string]any{"from": it.Feature, "to": f}
	it.Feature = f
	return Transition{Item: it, From: it.Stage, To: it.Stage, Kind: KindFeature, Data: data}, nil
}
