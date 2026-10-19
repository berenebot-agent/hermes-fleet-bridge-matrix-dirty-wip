package main

import (
	"strconv"
	"strings"
)

// Matrix clients render markdown only from a formatted_body, so an agent's plain
// `**bold**` shows up as literal asterisks. This renders the common markdown subset
// into the HTML Element sanitises. The text is escaped first because agent output is
// untrusted; tags are then emitted around the escaped text only.
//
// Each reply chunk is rendered on its own, so markup that straddles a chunk boundary
// degrades to literal text instead of producing unbalanced tags.
//
// shortcut: no lists, tables or backslash escapes, and emphasis must open and close on
// word boundaries. Add these only if agents are seen relying on them.
func matrixFormat(text string) string {
	var blocks, run, code []string
	fence := ""
	flush := func() {
		if len(run) > 0 {
			blocks = append(blocks, strings.Join(run, "<br/>"))
			run = nil
		}
	}
	for _, line := range strings.Split(escapeHTML(text), "\n") {
		trimmed := strings.TrimLeft(line, " ")
		if fence == "" && (strings.HasPrefix(trimmed, "```") || strings.HasPrefix(trimmed, "~~~")) {
			flush()
			fence = trimmed[:3]
			continue
		}
		if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				blocks = append(blocks, "<pre><code>"+strings.Join(code, "\n")+"</code></pre>")
				code, fence = nil, ""
				continue
			}
			code = append(code, line)
			continue
		}
		if trimmed == "" {
			// Keeps the blank line the plain-text body showed.
			run = append(run, "")
			continue
		}
		if tag, rest := headingTag(trimmed); tag != "" {
			flush()
			blocks = append(blocks, "<"+tag+">"+inlineHTML(rest)+"</"+tag+">")
			continue
		}
		if horizontalRule(trimmed) {
			flush()
			blocks = append(blocks, "<hr/>")
			continue
		}
		if quote := strings.TrimPrefix(trimmed, "&gt;"); quote != trimmed {
			flush()
			blocks = append(blocks, "<blockquote>"+inlineHTML(strings.TrimPrefix(quote, " "))+"</blockquote>")
			continue
		}
		run = append(run, inlineHTML(line))
	}
	// An unterminated fence still closes, so the HTML we send stays balanced.
	if fence != "" {
		blocks = append(blocks, "<pre><code>"+strings.Join(code, "\n")+"</code></pre>")
	}
	flush()
	return strings.Join(blocks, "<br/>")
}

var htmlEscaper = strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;")

func escapeHTML(s string) string { return htmlEscaper.Replace(s) }

// headingTag reports the heading tag for a "## Title" line, and its text.
func headingTag(line string) (string, string) {
	n := 0
	for n < len(line) && n < 6 && line[n] == '#' {
		n++
	}
	if n == 0 || n >= len(line) || line[n] != ' ' {
		return "", ""
	}
	return "h" + strconv.Itoa(n), strings.TrimSpace(line[n+1:])
}

func horizontalRule(line string) bool {
	line = strings.ReplaceAll(line, " ", "")
	if len(line) < 3 {
		return false
	}
	for i := 0; i < len(line); i++ {
		if line[i] != line[0] {
			return false
		}
	}
	return line[0] == '-' || line[0] == '*' || line[0] == '_'
}

func inlineHTML(s string) string {
	var b strings.Builder
	for i := 0; i < len(s); {
		switch {
		case s[i] == '`':
			if j := strings.IndexByte(s[i+1:], '`'); j > 0 {
				b.WriteString("<code>" + s[i+1:i+1+j] + "</code>")
				i += j + 2
				continue
			}
		case strings.HasPrefix(s[i:], "**") || strings.HasPrefix(s[i:], "__"):
			if j := emphasis(s, i, 2); j > 0 {
				b.WriteString("<b>" + inlineHTML(s[i+2:j]) + "</b>")
				i = j + 2
				continue
			}
		case strings.HasPrefix(s[i:], "~~"):
			if j := emphasis(s, i, 2); j > 0 {
				b.WriteString("<del>" + inlineHTML(s[i+2:j]) + "</del>")
				i = j + 2
				continue
			}
		case s[i] == '*' || s[i] == '_':
			if j := emphasis(s, i, 1); j > 0 {
				b.WriteString("<i>" + inlineHTML(s[i+1:j]) + "</i>")
				i = j + 1
				continue
			}
		case s[i] == '[':
			if text, url, n, ok := link(s[i:]); ok {
				b.WriteString(`<a href="` + url + `">` + inlineHTML(text) + "</a>")
				i += n
				continue
			}
		}
		b.WriteByte(s[i])
		i++
	}
	return b.String()
}

// emphasis finds the closing delimiter for one opening at s[start]. Requiring word
// boundaries on both ends is what keeps agent_purple-shirt-2, snake_case and 2*3*4
// intact instead of turning them italic.
func emphasis(s string, start, width int) int {
	delim := s[start : start+width]
	if start > 0 && isWord(s[start-1]) {
		return -1
	}
	for i := start + width; i+width <= len(s); i++ {
		if s[i:i+width] != delim {
			continue
		}
		if width == 1 && (s[i-1] == delim[0] || (i+1 < len(s) && s[i+1] == delim[0])) {
			continue // part of a longer run, not a closer
		}
		if s[i-1] == ' ' || s[i-1] == '\t' {
			continue // empty or space-padded emphasis
		}
		if i+width < len(s) && isWord(s[i+width]) {
			return -1
		}
		return i
	}
	return -1
}

func isWord(c byte) bool {
	return c == '_' || c >= '0' && c <= '9' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

// link parses "[label](url)" and reports how many bytes it consumed. Unsupported
// schemes stay plain text rather than becoming a link.
func link(s string) (string, string, int, bool) {
	close := strings.IndexByte(s, ']')
	if close < 1 || close+1 >= len(s) || s[close+1] != '(' {
		return "", "", 0, false
	}
	end := strings.IndexByte(s[close+2:], ')')
	if end < 0 {
		return "", "", 0, false
	}
	url := s[close+2 : close+2+end]
	if !safeURL(url) {
		return "", "", 0, false
	}
	return s[1:close], url, close + end + 3, true
}

func safeURL(url string) bool {
	if url == "" || strings.ContainsAny(url, " \"'<>") {
		return false
	}
	for _, scheme := range []string{"http://", "https://", "mxc://"} {
		if strings.HasPrefix(url, scheme) {
			return true
		}
	}
	return false
}
