package web

import (
	"strings"
	"testing"
)

func TestMarkdown(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"empty", "", ""},
		{"paragraph", "one\ntwo\n\nthree", "<p>one\ntwo</p>\n<p>three</p>\n"},
		{"headings", "# One\n## Two *x*\n###### Six\n####### seven", "<h1>One</h1>\n<h2>Two <em>x</em></h2>\n<h6>Six</h6>\n<p>####### seven</p>\n"},
		{"heading closing hashes", "## Two ##", "<h2>Two</h2>\n"},
		{"no space no heading", "#hashtag", "<p>#hashtag</p>\n"},
		{"emphasis", "**bold** and *em* and __b__ and _e_ and snake_case_name", "<p><strong>bold</strong> and <em>em</em> and <strong>b</strong> and <em>e</em> and snake_case_name</p>\n"},
		{"unclosed emphasis", "a * b and **c", "<p>a * b and **c</p>\n"},
		{"inline code", "run `go test ./...` now; ``a ` b``", "<p>run <code>go test ./...</code> now; <code>a ` b</code></p>\n"},
		{"unclosed code", "a `b", "<p>a `b</p>\n"},
		{"backslash escape", `\*not em\* and \[x\](y)`, "<p>*not em* and [x](y)</p>\n"},
		{"link", "see [the spec](https://example.com/a?b=1&c=2) and [rel](docs/x.md#y)", `<p>see <a href="https://example.com/a?b=1&amp;c=2" rel="noopener noreferrer">the spec</a> and <a href="docs/x.md#y" rel="noopener noreferrer">rel</a></p>` + "\n"},
		{"link with title", `[x](http://a.b "the title")`, `<p><a href="http://a.b" rel="noopener noreferrer">x</a></p>` + "\n"},
		{"link text formatted", "[**b** `c`](/board)", `<p><a href="/board" rel="noopener noreferrer"><strong>b</strong> <code>c</code></a></p>` + "\n"},
		{"link parens in url", "[w](https://en.wikipedia.org/wiki/Go_(language))", `<p><a href="https://en.wikipedia.org/wiki/Go_(language)" rel="noopener noreferrer">w</a></p>` + "\n"},
		{"not a link", "[x] and [y] (z)", "<p>[x] and [y] (z)</p>\n"},
		{"image is text", "![alt <b>](/board/x.png)", "<p>alt &lt;b&gt;</p>\n"},
		{"hr", "a\n\n---\n\n* * *", "<p>a</p>\n<hr>\n<hr>\n"},
		{"blockquote", "> quoted *x*\n> more\n\nafter", "<blockquote>\n<p>quoted <em>x</em>\nmore</p>\n</blockquote>\n<p>after</p>\n"},
		{"unordered list", "- one\n- two\n* three", "<ul>\n<li>one</li>\n<li>two</li>\n</ul>\n<ul>\n<li>three</li>\n</ul>\n"},
		{"ordered list", "1. one\n2. two", "<ol>\n<li>one</li>\n<li>two</li>\n</ol>\n"},
		{"ordered list start", "3) three\n4) four", "<ol start=\"3\">\n<li>three</li>\n<li>four</li>\n</ol>\n"},
		{"nested list", "- a\n  - b\n    1. c\n- d\n  more d", "<ul>\n<li>a\n<ul>\n<li>b\n<ol>\n<li>c</li>\n</ol></li>\n</ul></li>\n<li>d\nmore d</li>\n</ul>\n"},
		{"list then paragraph", "- a\n- b\n\nafter", "<ul>\n<li>a</li>\n<li>b</li>\n</ul>\n<p>after</p>\n"},
		{"list after paragraph", "Pieces:\n- a\n- b", "<p>Pieces:</p>\n<ul>\n<li>a</li>\n<li>b</li>\n</ul>\n"},
		{"fenced code", "```go\nfunc a() { return `<b>` }\n```\nafter", "<pre><code>func a() { return `&lt;b&gt;` }\n</code></pre>\n<p>after</p>\n"},
		{"tilde fence and markdown inside", "~~~\n# not a heading\n[x](javascript:alert(1))\n~~~", "<pre><code># not a heading\n[x](javascript:alert(1))\n</code></pre>\n"},
		{"unclosed fence", "```\nrest\nof it", "<pre><code>rest\nof it\n</code></pre>\n"},
		{"table", "| a | b |\n|---|:-:|\n| 1 | `x\\|y` |\n| **2** |\nnot a row", "<table>\n<thead>\n<tr><th>a</th><th>b</th></tr>\n</thead>\n<tbody>\n<tr><td>1</td><td><code>x|y</code></td></tr>\n<tr><td><strong>2</strong></td><td></td></tr>\n</tbody>\n</table>\n<p>not a row</p>\n"},
		{"table without outer pipes", "a | b\n--- | ---\n1 | 2", "<table>\n<thead>\n<tr><th>a</th><th>b</th></tr>\n</thead>\n<tbody>\n<tr><td>1</td><td>2</td></tr>\n</tbody>\n</table>\n"},
		{"pipe without delimiter row", "a | b\nc | d", "<p>a | b\nc | d</p>\n"},
		{"crlf", "# A\r\n\r\nb\r\n", "<h1>A</h1>\n<p>b</p>\n"},

		// What must never get through: raw HTML, script URLs, attribute breaking.
		{"script tag", "<script>alert(1)</script>", "<p>&lt;script&gt;alert(1)&lt;/script&gt;</p>\n"},
		{"img onerror", `<img src=x onerror="alert(1)">`, "<p>&lt;img src=x onerror=&#34;alert(1)&#34;&gt;</p>\n"},
		{"html block", "<div>\n<b>x</b>\n</div>", "<p>&lt;div&gt;\n&lt;b&gt;x&lt;/b&gt;\n&lt;/div&gt;</p>\n"},
		{"html in heading", "# <i>x</i>", "<h1>&lt;i&gt;x&lt;/i&gt;</h1>\n"},
		{"html in list and table", "- <b>\n\n| <x> |\n|---|\n| <y> |", "<ul>\n<li>&lt;b&gt;</li>\n</ul>\n<table>\n<thead>\n<tr><th>&lt;x&gt;</th></tr>\n</thead>\n<tbody>\n<tr><td>&lt;y&gt;</td></tr>\n</tbody>\n</table>\n"},
		{"javascript url", "[x](javascript:alert(1))", "<p>x</p>\n"},
		{"javascript url mixed case", "[x](JaVaScRiPt:alert(1))", "<p>x</p>\n"},
		{"javascript url leading space", "[x]( javascript:alert(1))", "<p>x</p>\n"},
		{"data url", "[x](data:text/html;base64,PHNjcmlwdD4=)", "<p>x</p>\n"},
		{"vbscript url", "[x](vbscript:msgbox)", "<p>x</p>\n"},
		{"mailto is refused too", "[x](mailto:a@b.c)", "<p>x</p>\n"},
		{"entity-encoded scheme stays a relative link", "[x](&#106;avascript:alert(1))", "<p>x</p>\n"},
		{"control character in url", "[x](java\x01script:alert(1))", "<p>x</p>\n"},
		{"quote breaking href", `[x](https://a.b/"onmouseover="alert(1))`, `<p><a href="https://a.b/&#34;onmouseover=&#34;alert(1)" rel="noopener noreferrer">x</a></p>` + "\n"},
		{"single quote in href", `[x](https://a.b/'x)`, `<p><a href="https://a.b/&#39;x" rel="noopener noreferrer">x</a></p>` + "\n"},
		{"angle brackets in href", `[x](https://a.b/<script>)`, `<p><a href="https://a.b/&lt;script&gt;" rel="noopener noreferrer">x</a></p>` + "\n"},
		{"html inside code", "`<script>` and\n\n```\n</code></pre><script>alert(1)</script>\n```", "<p><code>&lt;script&gt;</code> and</p>\n<pre><code>&lt;/code&gt;&lt;/pre&gt;&lt;script&gt;alert(1)&lt;/script&gt;\n</code></pre>\n"},
		{"entities are text", "&amp; &lt;b&gt; &#60;", "<p>&amp;amp; &amp;lt;b&amp;gt; &amp;#60;</p>\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := string(markdown(tc.in)); got != tc.want {
				t.Errorf("markdown(%q)\n got %q\nwant %q", tc.in, got, tc.want)
			}
		})
	}
}

// Whatever goes in, the only tags that come out are the allow-list's, and no attribute but href
// (http, https or relative), rel and an ordered list's start.
func TestMarkdownAllowList(t *testing.T) {
	inputs := []string{
		"<script>alert(1)</script>", "<a href=\"javascript:x\">y</a>", "[a](javascript:b)", "![a](b)",
		"```\n<x>\n```", "| <a> | b |\n|--|--|\n| c | <d> |", "- <e>\n  - [f](data:g)", "> <h>",
		"**<i>**", "`<j>`", "[<k>](https://l)", "[m](https://n\"o)", "<!-- p -->", "<![CDATA[q]]>",
		"[r](//s)", "[t](\\\\u)", "[v](HTTPS://W)", "_<x>_",
	}
	for _, in := range inputs {
		checkAllowList(t, in)
	}
}

func FuzzMarkdown(f *testing.F) {
	for _, in := range []string{"# a\n\n- b\n  - [c](https://d)\n\n| e | f |\n|---|---|\n| `g` | **h** |\n\n```\n<i>\n```", "[a](javascript:b)", "<script>"} {
		f.Add(in)
	}
	f.Fuzz(checkAllowList)
}

var allowedTags = map[string]bool{}

func init() {
	for _, tag := range []string{"h1", "h2", "h3", "h4", "h5", "h6", "p", "ul", "ol", "li", "table", "thead", "tbody",
		"tr", "th", "td", "pre", "code", "em", "strong", "a", "hr", "blockquote"} {
		allowedTags[tag] = true
	}
}

func checkAllowList(t *testing.T, in string) {
	out := string(markdown(in))
	for rest := out; ; {
		i := strings.IndexByte(rest, '<')
		if i < 0 {
			return
		}
		end := strings.IndexByte(rest[i:], '>')
		if end < 0 {
			t.Errorf("%q: unclosed tag in %q", in, out)
			return
		}
		tag := strings.TrimPrefix(rest[i+1:i+end], "/")
		name, attrs, _ := strings.Cut(tag, " ")
		if !allowedTags[name] {
			t.Errorf("%q: tag %q in %q", in, name, out)
		}
		switch {
		case attrs == "":
		case name == "a" && strings.HasPrefix(attrs, `href="`) && strings.HasSuffix(attrs, `" rel="noopener noreferrer"`):
			href := strings.TrimSuffix(strings.TrimPrefix(attrs, `href="`), `" rel="noopener noreferrer"`)
			if strings.ContainsAny(href, `"<>'`) {
				t.Errorf("%q: href %q", in, href)
			}
			if scheme, _, ok := strings.Cut(href, ":"); ok && !strings.ContainsAny(scheme, "/?#") &&
				strings.ToLower(scheme) != "http" && strings.ToLower(scheme) != "https" {
				t.Errorf("%q: href scheme %q", in, scheme)
			}
		case name == "ol" && strings.HasPrefix(attrs, `start="`):
		default:
			t.Errorf("%q: attributes %q on %s", in, attrs, name)
		}
		rest = rest[i+end+1:]
	}
}
