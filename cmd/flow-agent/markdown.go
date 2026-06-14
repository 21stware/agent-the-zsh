package main

import (
	"bytes"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/alecthomas/chroma/v2/quick"
)

// renderMarkdown converts markdown to ANSI, handling fenced code blocks
// (with syntax highlighting), tables (aligned), headings, bullets, and inline spans.
func renderMarkdown(s string) string {
	var out strings.Builder
	var codeBuf strings.Builder
	var tableBuf []string
	inCode := false
	codeLang := ""

	flushTable := func() {
		if len(tableBuf) > 0 {
			out.WriteString(renderTable(tableBuf) + "\n")
			tableBuf = nil
		}
	}

	for _, ln := range strings.Split(s, "\n") {
		if strings.HasPrefix(ln, "```") {
			flushTable()
			if !inCode {
				inCode = true
				codeLang = strings.TrimSpace(strings.TrimPrefix(ln, "```"))
				codeBuf.Reset()
			} else {
				inCode = false
				out.WriteString(highlightCode(codeLang, codeBuf.String()) + "\n")
				codeLang = ""
				codeBuf.Reset()
			}
			continue
		}
		if inCode {
			codeBuf.WriteString(ln + "\n")
			continue
		}
		if isTableLine(ln) {
			tableBuf = append(tableBuf, ln)
			continue
		}
		flushTable()
		out.WriteString(renderMarkdownLine(ln) + "\n")
	}
	if inCode && codeBuf.Len() > 0 {
		out.WriteString(highlightCode(codeLang, codeBuf.String()))
	}
	flushTable()
	return strings.TrimRight(out.String(), "\n")
}

func isTableLine(ln string) bool {
	return strings.HasPrefix(strings.TrimLeft(ln, " \t"), "|")
}

// renderTable renders buffered table rows with aligned columns and box borders.
func renderTable(rows []string) string {
	type trow struct {
		cells []string
		sep   bool
	}
	var parsed []trow
	maxCols := 0
	for _, ln := range rows {
		if reTableSep.MatchString(strings.TrimSpace(ln)) {
			parsed = append(parsed, trow{sep: true})
			continue
		}
		parts := strings.Split(strings.TrimSpace(ln), "|")
		if len(parts) > 0 && parts[0] == "" {
			parts = parts[1:]
		}
		if len(parts) > 0 && parts[len(parts)-1] == "" {
			parts = parts[:len(parts)-1]
		}
		cells := make([]string, len(parts))
		for i, p := range parts {
			cells[i] = strings.TrimSpace(p)
		}
		parsed = append(parsed, trow{cells: cells})
		if len(cells) > maxCols {
			maxCols = len(cells)
		}
	}
	widths := make([]int, maxCols)
	for _, r := range parsed {
		if r.sep {
			continue
		}
		for j, cell := range r.cells {
			if j < maxCols {
				if w := utf8.RuneCountInString(cell); w > widths[j] {
					widths[j] = w
				}
			}
		}
	}
	var sb strings.Builder
	isHeader := true
	for _, r := range parsed {
		if r.sep {
			sb.WriteString(cDim + "├")
			for j, w := range widths {
				if j > 0 {
					sb.WriteString("┼")
				}
				sb.WriteString(strings.Repeat("─", w+2))
			}
			sb.WriteString("┤" + cReset + "\n")
			isHeader = false
			continue
		}
		sb.WriteString(cDim + "│" + cReset)
		for j := 0; j < maxCols; j++ {
			cell := ""
			if j < len(r.cells) {
				cell = r.cells[j]
			}
			rendered := applyInline(cell)
			if isHeader {
				rendered = cBold + rendered + cReset
			}
			pad := widths[j] - utf8.RuneCountInString(cell)
			sb.WriteString(" " + rendered + strings.Repeat(" ", pad) + " ")
			sb.WriteString(cDim + "│" + cReset)
		}
		sb.WriteString("\n")
	}
	return strings.TrimRight(sb.String(), "\n")
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
	reBold     = regexp.MustCompile(`\*\*([^*]+)\*\*`)
	reItalic   = regexp.MustCompile(`(^|[^*])\*([^*\s][^*]*)\*`)
	reUnder    = regexp.MustCompile(`(^|[^_])_([^_\s][^_]*)_`)
	reCode     = regexp.MustCompile("`([^`]+)`")
	reH        = regexp.MustCompile(`^(#{1,6})\s+(.*)$`)
	reBullet   = regexp.MustCompile(`^(\s*)([-*+])\s+(.*)$`)
	reTableSep = regexp.MustCompile(`^\|[\s|:\-]+\|$`)
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
