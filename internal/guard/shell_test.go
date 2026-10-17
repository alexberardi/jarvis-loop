package guard

import (
	"reflect"
	"testing"
)

func TestParse(t *testing.T) {
	cases := []struct {
		in   string
		segs [][]string
		subs []string
	}{
		{"go test ./...", [][]string{{"go", "test", "./..."}}, nil},
		{"a && b || c; d | e & f", [][]string{{"a"}, {"b"}, {"c"}, {"d"}, {"e"}, {"f"}}, nil},
		{`echo "a b" 'c d' e\ f`, [][]string{{"echo", "a b", "c d", "e f"}}, nil},
		{"rm -rf x 2>/dev/null >> log.txt", [][]string{{"rm", "-rf", "x"}}, nil},
		{"make 2>&1 | tee out", [][]string{{"make"}, {"tee", "out"}}, nil},
		{"echo $(git push origin main) `id`", [][]string{{"echo", "$SUB", "$SUB"}}, []string{"git push origin main", "id"}},
		{`echo "x $(kill 1) y"`, [][]string{{"echo", "x $SUB y"}}, []string{"kill 1"}},
		{"(cd sub && make)", [][]string{{"cd", "sub"}, {"make"}}, nil},
		{"diff <(sort a) b", [][]string{{"diff", "b"}}, []string{"sort a"}},
		{"cat <<'EOF' > f.txt\nkill everything\nEOF\ngo test", [][]string{{"cat"}, {"go", "test"}}, nil},
		{"cat <<-EOF\n\tsudo x\n\tEOF\n", [][]string{{"cat"}}, nil},
		{"ls # sudo rm -rf /", [][]string{{"ls"}}, nil},
		{"a \\\n b", [][]string{{"a", "b"}}, nil},
	}
	for _, c := range cases {
		segs, subs, err := parse(c.in)
		if err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		if !reflect.DeepEqual(segs, c.segs) || !reflect.DeepEqual(subs, c.subs) {
			t.Errorf("%q:\n segs %q want %q\n subs %q want %q", c.in, segs, c.segs, subs, c.subs)
		}
	}
	for _, bad := range []string{"echo 'x", `echo "x`, "echo $(x", "echo `x", "cat <<EOF"} {
		if _, _, err := parse(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

func TestParseScriptKeepsRedirectionsAndHeredocs(t *testing.T) {
	sc, err := parseScript("cat <<'A' > out.md; python3 <<B\nbody a\nA\nbody b\nB\ngrep x < in.txt <<< 'here' || true")
	if err != nil {
		t.Fatal(err)
	}
	want := []segment{
		{words: []string{"cat"}, targets: []string{"out.md"}, bodies: []heredocBody{{text: "body a\n", quoted: true}}},
		{words: []string{"python3"}, bodies: []heredocBody{{text: "body b\n"}}},
		{words: []string{"grep", "x"}, targets: []string{"in.txt", "here"}},
		{words: []string{"true"}},
	}
	if !reflect.DeepEqual(sc.segs, want) || sc.piped {
		t.Fatalf("segs = %#v, piped = %v", sc.segs, sc.piped)
	}
	for _, s := range []string{"a | b", "a |& b"} {
		if sc, _ := parseScript(s); !sc.piped {
			t.Errorf("%q: not piped", s)
		}
	}
}
