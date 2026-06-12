package main

import (
	"regexp"
	"strings"
)

// renderMarkdown converts a block of lightweight markdown to ANSI for terminal
// display: **bold**, *italic*/_italic_, `code`, # headings, and - / * / 1.
// bullets. It is deliberately simple (line-oriented, no nested parsing) and
// degrades to plain text when NO_COLOR is set (the c* vars are empty then).
func renderMarkdown(s string) string {
	lines := strings.Split(s, "\n")
	for i, ln := range lines {
		lines[i] = renderMarkdownLine(ln)
	}
	return strings.Join(lines, "\n")
}

var (
	reBold   = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalic = regexp.MustCompile(`(^|[^*])\*([^*\s][^*]*)\*`)
	reUnder  = regexp.MustCompile(`(^|[^_])_([^_\s][^_]*)_`)
	reCode   = regexp.MustCompile("`([^`]+)`")
	reH      = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reBullet = regexp.MustCompile(`^(\s*)([-*+])\s+(.*)$`)
)

func renderMarkdownLine(ln string) string {
	// Headings: render as bold (+ cyan), drop the leading #s.
	if m := reH.FindStringSubmatch(ln); m != nil {
		return cBold + cCyan + applyInline(m[2]) + cReset
	}
	// Bullets: normalize marker to "•", keep indentation.
	if m := reBullet.FindStringSubmatch(ln); m != nil {
		return m[1] + cDim + "• " + cReset + applyInline(m[3])
	}
	return applyInline(ln)
}

// applyInline handles inline spans: code first (so its contents aren't further
// formatted), then bold, then italic.
func applyInline(s string) string {
	s = reCode.ReplaceAllString(s, cYellow+"$1"+cReset)
	s = reBold.ReplaceAllString(s, cBold+"$1"+cReset)
	s = reItalic.ReplaceAllString(s, "$1"+"\033[3m"+"$2"+cReset)
	s = reUnder.ReplaceAllString(s, "$1"+"\033[3m"+"$2"+cReset)
	return s
}
