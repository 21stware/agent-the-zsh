package classify

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// sample is one labeled test input.
type sample struct {
	input  string
	label  Label
	source string
}

// --- loaders for the three test-set sources ---

// loadNL2Bash reads the NL side of nl2bash. Every line is a natural-language
// description of a task, i.e. label NL. We cap the count and filter out the
// "(GNU specific)" / "(BSD specific)" parenthetical prefixes that are metadata,
// not user input.
func loadNL2Bash(t *testing.T, max int) []sample {
	path := filepath.Join("..", "..", "testdata", "raw", "nl2bash.nl")
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("nl2bash data not present (%v); run cmd/genset or curl", err)
	}
	defer f.Close()
	paren := regexp.MustCompile(`^\([^)]*\)\s*`)
	var out []sample
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1024*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = paren.ReplaceAllString(line, "")
		if len(line) < 8 {
			continue
		}
		out = append(out, sample{input: line, label: NL, source: "nl2bash"})
		if max > 0 && len(out) >= max {
			break
		}
	}
	return out
}

// tldrPlaceholder matches the {{path/to/file}} template syntax used by tldr.
var tldrPlaceholder = regexp.MustCompile(`\{\{([^}]*)\}\}`)

// loadTLDR reads command examples extracted from tldr pages. Each line is a
// real shell command (label CMD), wrapped in backticks with {{placeholders}}.
// We strip backticks and substitute placeholders with a representative concrete
// token so the line parses as a plausible command.
func loadTLDR(t *testing.T) []sample {
	path := filepath.Join("..", "..", "testdata", "raw", "tldr.cmds")
	f, err := os.Open(path)
	if err != nil {
		t.Skipf("tldr data not present (%v)", err)
	}
	defer f.Close()
	var out []sample
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		line = strings.Trim(line, "`")
		if line == "" {
			continue
		}
		line = tldrPlaceholder.ReplaceAllStringFunc(line, func(m string) string {
			inner := tldrPlaceholder.FindStringSubmatch(m)[1]
			return concretizePlaceholder(inner)
		})
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		out = append(out, sample{input: line, label: CMD, source: "tldr"})
	}
	return out
}

// concretizePlaceholder turns a tldr placeholder body into a concrete token.
// "path/to/file" -> "file.txt"; "-C|--directory" -> "-C"; numbers stay; etc.
func concretizePlaceholder(inner string) string {
	inner = strings.TrimSpace(inner)
	// option alternation like [-C|--directory] or -C|--directory
	if strings.Contains(inner, "|") {
		first := strings.SplitN(inner, "|", 2)[0]
		first = strings.Trim(first, "[]")
		return strings.TrimSpace(first)
	}
	switch {
	case strings.Contains(inner, "path/to/directory"):
		return "dir"
	case strings.Contains(inner, "path/to"):
		// keep an extension hint if present
		base := inner[strings.LastIndex(inner, "/")+1:]
		if base == "" || base == "file" {
			return "file.txt"
		}
		return base
	case inner == "":
		return "x"
	default:
		// collapse internal spaces; take a single representative token
		fields := strings.Fields(inner)
		if len(fields) > 0 {
			return strings.Trim(fields[0], "[]")
		}
		return "x"
	}
}

// loadAdversarial reads the hand-built gray-zone set: "<LABEL>\t<input>".
func loadAdversarial(t *testing.T) []sample {
	path := filepath.Join("..", "..", "testdata", "adversarial.txt")
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("adversarial data missing: %v", err)
	}
	defer f.Close()
	var out []sample
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		if strings.HasPrefix(strings.TrimSpace(line), "#") || strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 2)
		if len(parts) != 2 {
			continue
		}
		var lab Label
		switch strings.TrimSpace(parts[0]) {
		case "CMD":
			lab = CMD
		case "NL":
			lab = NL
		default:
			continue
		}
		out = append(out, sample{input: parts[1], label: lab, source: "adversarial"})
	}
	return out
}

// --- the accuracy baseline test ---

type scoreboard struct {
	total, correct   int
	cmdAsNL, nlAsCMD int // the two error directions
	misexamples      []string
}

func (s *scoreboard) record(smp sample, got Label) {
	s.total++
	if got == smp.label {
		s.correct++
		return
	}
	if smp.label == CMD && got == NL {
		s.cmdAsNL++ // the dangerous error: a command sent to the network
	} else {
		s.nlAsCMD++ // the safe error: NL run as a command, caught downstream
	}
	if len(s.misexamples) < 40 {
		s.misexamples = append(s.misexamples,
			fmt.Sprintf("    [%s] want=%s got=%s: %q", smp.source, smp.label, got, smp.input))
	}
}

func (s scoreboard) acc() float64 {
	if s.total == 0 {
		return 0
	}
	return 100 * float64(s.correct) / float64(s.total)
}

func TestAccuracyBaseline(t *testing.T) {
	c := New(knownCommandsForTest)

	var all []sample
	all = append(all, loadAdversarial(t)...)
	all = append(all, loadTLDR(t)...)
	all = append(all, loadNL2Bash(t, 4000)...) // cap NL to keep classes balanced-ish

	bySource := map[string]*scoreboard{}
	overall := &scoreboard{}
	for _, smp := range all {
		if bySource[smp.source] == nil {
			bySource[smp.source] = &scoreboard{}
		}
		got := c.Classify(smp.input).Label
		bySource[smp.source].record(smp, got)
		overall.record(smp, got)
	}

	sources := make([]string, 0, len(bySource))
	for k := range bySource {
		sources = append(sources, k)
	}
	sort.Strings(sources)

	t.Logf("=== flow stage-0 classifier — accuracy baseline ===")
	for _, src := range sources {
		sb := bySource[src]
		t.Logf("%-12s n=%-5d acc=%6.2f%%  (cmd→NL err=%d, NL→cmd err=%d)",
			src, sb.total, sb.acc(), sb.cmdAsNL, sb.nlAsCMD)
	}
	t.Logf("%-12s n=%-5d acc=%6.2f%%  (cmd→NL err=%d, NL→cmd err=%d)",
		"OVERALL", overall.total, overall.acc(), overall.cmdAsNL, overall.nlAsCMD)

	// The dangerous-error rate: real commands misrouted to NL. This is the
	// metric the design says to minimize.
	cmdTotal := bySource["tldr"].total + countLabel(all, CMD, "adversarial")
	dangerRate := 100 * float64(overall.cmdAsNL) / float64(cmdTotal)
	t.Logf("DANGEROUS cmd→NL rate over all CMD inputs: %.2f%% (%d/%d)",
		dangerRate, overall.cmdAsNL, cmdTotal)

	t.Logf("--- sample misclassifications ---")
	for _, m := range overall.misexamples {
		t.Log(m)
	}

	// Guardrails so regressions fail the build. Loose for now; tighten as the
	// cascade improves. These encode the design priority: keep cmd→NL low.
	if overall.acc() < 80 {
		t.Errorf("overall accuracy %.2f%% below 80%% floor", overall.acc())
	}
	if dangerRate > 5 {
		t.Errorf("dangerous cmd→NL rate %.2f%% exceeds 5%% ceiling", dangerRate)
	}
}

func countLabel(all []sample, lab Label, source string) int {
	n := 0
	for _, s := range all {
		if s.source == source && s.label == lab {
			n++
		}
	}
	return n
}

// TestMultiLineCommands ensures multi-line shell input is always classified as
// CMD. In zsh, multi-line $BUFFER only occurs for incomplete shell constructs
// (line continuations, for/if/while blocks, heredocs, function definitions).
// Natural language is always single-line.
func TestMultiLineCommands(t *testing.T) {
	c := New(knownCommandsForTest)

	cases := []struct {
		input  string
		reason string
	}{
		{"echo hello \\\nworld", "line continuation"},
		{"for i in 1 2 3; do\n  echo $i\ndone", "for loop"},
		{"if [ -f file ]; then\n  rm file\nfi", "if block"},
		{"while true; do\n  echo hi\ndone", "while loop"},
		{"case $x in\n  a) echo a ;;\n  b) echo b ;;\nesac", "case block"},
		{"foo() {\n  echo bar\n}", "function definition"},
		{"echo hello\necho world", "two commands"},
		{"git add .\ngit commit -m \"msg\"\ngit push", "three commands"},
		{"cat <<EOF\nhello\nworld\nEOF", "heredoc"},
		{"docker run -d \\\n  --name myapp \\\n  -p 8080:80 \\\n  nginx:latest", "docker with continuations"},
		{"echo \"hello\nworld\"", "newline in quotes"},
		{"# comment\necho hello", "comment + command"},
	}

	for _, tc := range cases {
		got := c.Classify(tc.input)
		if got.Label != CMD {
			t.Errorf("multi-line [%s] %q: got %s, want CMD (reason=%s)", tc.reason, tc.input, got.Label, got.Reason)
		}
		if got.Reason != "multi-line" {
			t.Errorf("multi-line [%s] %q: reason=%s, want multi-line", tc.reason, tc.input, got.Reason)
		}
	}
}
