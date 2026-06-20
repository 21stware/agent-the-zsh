package main

import (
	"bytes"
	"os"
	"regexp"
	"strings"
	"sync"

	"github.com/alecthomas/chroma/v2/quick"
	"github.com/charmbracelet/glamour"
)

var (
	glamourRenderer *glamour.TermRenderer
	glamourOnce     sync.Once
)

func getGlamourRenderer() *glamour.TermRenderer {
	glamourOnce.Do(func() {
		var opts []glamour.TermRendererOption
		if isTTY && os.Getenv("NO_COLOR") == "" {
			opts = append(opts, glamour.WithAutoStyle())
		} else {
			opts = append(opts, glamour.WithStandardStyle("notty"))
		}
		opts = append(opts, glamour.WithWordWrap(0))
		r, err := glamour.NewTermRenderer(opts...)
		if err == nil {
			glamourRenderer = r
		}
	})
	return glamourRenderer
}

// renderMarkdown converts markdown to ANSI using glamour, which handles
// tables (with proper CJK width), code blocks, headings, lists, and inline
// spans with a mature, well-tested renderer.
func renderMarkdown(s string) string {
	r := getGlamourRenderer()
	if r == nil {
		return s
	}
	out, err := r.Render(s)
	if err != nil {
		return s
	}
	return strings.TrimRight(out, "\n")
}

func isTableLine(ln string) bool {
	return strings.HasPrefix(strings.TrimLeft(ln, " \t"), "|")
}

// highlightCode applies chroma syntax highlighting for the terminal.
// Falls back to plain yellow on NO_COLOR or unknown language.
func highlightCode(lang, code string) string {
	code = strings.TrimRight(code, "\n")
	if cReset == "" {
		return code
	}
	var buf bytes.Buffer
	if err := quick.Highlight(&buf, code, lang, "terminal256", "monokai"); err != nil {
		return cYellow + code + cReset
	}
	return strings.TrimRight(buf.String(), "\n")
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
	if m := reH.FindStringSubmatch(ln); m != nil {
		return cBold + cCyan + applyInline(m[2]) + cReset
	}
	if m := reBullet.FindStringSubmatch(ln); m != nil {
		return m[1] + cDim + "• " + cReset + applyInline(m[3])
	}
	return applyInline(ln)
}

func applyInline(s string) string {
	s = reCode.ReplaceAllString(s, cYellow+"$1"+cReset)
	s = reBold.ReplaceAllString(s, cBold+"$1"+cReset)
	s = reItalic.ReplaceAllString(s, "$1"+"\033[3m"+"$2"+cReset)
	s = reUnder.ReplaceAllString(s, "$1"+"\033[3m"+"$2"+cReset)
	return s
}
