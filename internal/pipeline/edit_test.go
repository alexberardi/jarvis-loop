package pipeline

import (
	"reflect"
	"strings"
	"testing"
)

func TestApplyEdit(t *testing.T) {
	title, ask, empty := " new title ", "new ask", " "
	it := item(Triage)
	it.Ask = "old ask"
	tr, err := ApplyEdit(it, &title, &ask)
	wantCode(t, err, "")
	if tr.Item.Title != "new title" || tr.Item.Ask != "new ask" || tr.Kind != KindEdited || tr.From != Triage || tr.To != Triage {
		t.Fatalf("edit: %+v", tr)
	}
	want := map[string]any{"title": map[string]string{"from": "t", "to": "new title"}, "ask": map[string]string{"from": "old ask", "to": "new ask"}}
	if !reflect.DeepEqual(tr.Data, want) {
		t.Fatalf("edit data (the old text): %v", tr.Data)
	}
	// An empty ask is allowed (the title says it all); an empty title is not.
	tr, err = ApplyEdit(it, nil, &empty)
	wantCode(t, err, "")
	if tr.Item.Ask != "" || tr.Item.Title != "t" {
		t.Fatalf("clearing the ask: %+v", tr.Item)
	}
	_, err = ApplyEdit(it, &empty, nil)
	wantCode(t, err, CodeInvalidField)
	_, err = ApplyEdit(it, nil, nil)
	wantCode(t, err, CodeMissingField)

	// Only before build starts: triage, or a triage question.
	q := item(NeedsYou)
	q.ReturnStage = Triage
	_, err = ApplyEdit(q, &title, nil)
	wantCode(t, err, "")
	for _, s := range []Stage{Build, Review, Land, Held, QA, Done, Closed} {
		_, err = ApplyEdit(item(s), &title, nil)
		wantCode(t, err, CodeWrongStage)
	}
	q.ReturnStage = Build
	_, err = ApplyEdit(q, nil, &ask)
	wantCode(t, err, CodeWrongStage)
}

func TestApplyUnlink(t *testing.T) {
	it := item(Triage)
	it.DependsOn = []int64{3, 4}
	tr, err := ApplyUnlink(it, 3)
	wantCode(t, err, "")
	if !reflect.DeepEqual(tr.Item.DependsOn, []int64{4}) || tr.Kind != KindUnlinked || tr.Data["after"] != int64(3) {
		t.Fatalf("unlink: %+v", tr)
	}
	if !reflect.DeepEqual(it.DependsOn, []int64{3, 4}) {
		t.Fatalf("unlink changed its input: %v", it.DependsOn)
	}
	_, err = ApplyUnlink(it, 5)
	wantCode(t, err, CodeInvalidField)
	for _, s := range []Stage{Done, Closed} {
		d := item(s)
		d.DependsOn = []int64{3}
		_, err = ApplyUnlink(d, 3)
		wantCode(t, err, CodeWrongStage)
	}
}

func TestApplyResources(t *testing.T) {
	it := item(Build)
	it.Resources = []string{"macos"}
	tr, err := ApplyResources(it, "voice, macos,voice")
	wantCode(t, err, "")
	if !reflect.DeepEqual(tr.Item.Resources, []string{"macos", "voice"}) || tr.Kind != KindResources {
		t.Fatalf("resources: %+v", tr)
	}
	if want := map[string]any{"from": []string{"macos"}, "to": []string{"macos", "voice"}}; !reflect.DeepEqual(tr.Data, want) {
		t.Fatalf("resources data: %v", tr.Data)
	}
	for _, raw := range []string{"", "none"} {
		tr, err = ApplyResources(it, raw)
		wantCode(t, err, "")
		if len(tr.Item.Resources) != 0 || tr.Item.Resources == nil {
			t.Fatalf("resources %q: %v", raw, tr.Item.Resources)
		}
	}
	_, err = ApplyResources(it, "macos,pi")
	wantCode(t, err, CodeInvalidField)
	// Triage sets them itself; done and closed items need none.
	for _, s := range []Stage{Triage, Done, Closed} {
		_, err = ApplyResources(item(s), "macos")
		wantCode(t, err, CodeWrongStage)
	}
	for _, s := range []Stage{Review, Land, NeedsYou, Held} {
		_, err = ApplyResources(item(s), "macos")
		wantCode(t, err, "")
	}
}

func TestFeatureSlug(t *testing.T) {
	cases := map[string]string{
		"e2e":                     "e2e",
		" Smallest Blocks ":       "smallest-blocks",
		"smallest-blocks":         "smallest-blocks",
		"Pantry: scan & stock v2": "pantry-scan-stock-v2",
		"  ":                      "",
		"!!":                      "",
		strings.Repeat("ab ", 40): strings.TrimSuffix(strings.Repeat("ab-", 16), "-"),
	}
	for in, want := range cases {
		if got := FeatureSlug(in); got != want {
			t.Errorf("FeatureSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestApplyFeature(t *testing.T) {
	it := item(Build)
	it.Feature = "old"
	tr, err := ApplyFeature(it, " Smallest Blocks")
	wantCode(t, err, "")
	if tr.Item.Feature != "smallest-blocks" || tr.Kind != KindFeature || tr.From != Build || tr.To != Build {
		t.Fatalf("feature: %+v", tr)
	}
	if want := map[string]any{"from": "old", "to": "smallest-blocks"}; !reflect.DeepEqual(tr.Data, want) {
		t.Fatalf("feature data: %v", tr.Data)
	}
	// "" and none clear it.
	for _, raw := range []string{"", " none "} {
		tr, err = ApplyFeature(it, raw)
		wantCode(t, err, "")
		if tr.Item.Feature != "" {
			t.Fatalf("clear with %q: %+v", raw, tr.Item)
		}
	}
	_, err = ApplyFeature(it, "!!")
	wantCode(t, err, CodeInvalidField)
	// Any stage: a feature's progress counts its done items too.
	for _, s := range []Stage{Triage, NeedsYou, Held, QA, Done, Closed} {
		_, err = ApplyFeature(item(s), "x")
		wantCode(t, err, "")
	}
}
