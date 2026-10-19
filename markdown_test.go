package main

import "testing"

func TestMatrixFormat(t *testing.T) {
	cases := []struct{ name, in, want string }{
		{"bold", "Here are the main stories:\n\n**Top Stories:**\n\n- **France in turmoil**: riots.", "Here are the main stories:<br/><br/><b>Top Stories:</b><br/><br/>- <b>France in turmoil</b>: riots."},
		{"italic and code", "*see* the `data/matrix-media` path", "<i>see</i> the <code>data/matrix-media</code> path"},
		{"strikethrough", "~~old~~ new", "<del>old</del> new"},
		{"identifiers untouched", "agent_purple-shirt-2 writes API_SERVER_KEY for hermes_fleet", "agent_purple-shirt-2 writes API_SERVER_KEY for hermes_fleet"},
		{"intraword asterisks untouched", "2*3*4 and a*b", "2*3*4 and a*b"},
		{"unclosed emphasis literal", "unclosed **bold", "unclosed **bold"},
		{"html escaped", `<script>alert("x") & co</script>`, "&lt;script&gt;alert(\"x\") &amp; co&lt;/script&gt;"},
		{"heading", "## Summary\ntext", "<h2>Summary</h2><br/>text"},
		{"hashtag not heading", "#hashtag stays", "#hashtag stays"},
		{"rule", "a\n\n---\n\nb", "a<br/><br/><hr/><br/><br/>b"},
		{"quote", "> quoted **x**", "<blockquote>quoted <b>x</b></blockquote>"},
		{"fence keeps markup literal", "```\n**x** <y>\n```", "<pre><code>**x** &lt;y&gt;</code></pre>"},
		{"unterminated fence closes", "```\nx", "<pre><code>x</code></pre>"},
		{"safe link", "see [docs](https://example.com/a) now", `see <a href="https://example.com/a">docs</a> now`},
		{"unsafe scheme stays text", "[x](javascript:alert(1))", "[x](javascript:alert(1))"},
		{"empty", "", ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := matrixFormat(c.in); got != c.want {
				t.Errorf("matrixFormat(%q)\n got %q\nwant %q", c.in, got, c.want)
			}
		})
	}
}
