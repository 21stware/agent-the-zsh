// Package classify implements stage-0 of flow's decision cascade: a fast,
// offline, network-free rule layer that decides whether a line of input is a
// shell command (CMD) or natural language (NL).
//
// Design constraints (from the project spec, non-negotiable):
//   - Lean CMD when ambiguous. Misclassifying a command as NL is far worse
//     than the reverse, because the CMD path must stay zero-latency.
//   - Never depend on the network here.
//
// The cascade, in order:
//  1. Empty input -> CMD (it's a no-op accept-line).
//  2. Parse with mvdan/sh. Structured shell (assignments, keywords like for/if,
//     subshells, pipes, redirections, expansions) -> CMD.
//  3. First word is a known command/builtin/alias and NOT a gray-zone verb -> CMD.
//  4. First word is a known gray-zone verb (make/find/time/...): CMD unless the
//     argument portion reads like an English/Chinese sentence.
//  5. First word unknown: NL only on a strong NL signal; otherwise lean CMD
//     (a single unknown token is more likely a typo/uninstalled tool than NL).
package classify

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Label is the classification outcome.
type Label int

const (
	CMD Label = iota // run as a shell command, zero latency
	NL               // natural language, hand to the translator
)

func (l Label) String() string {
	if l == NL {
		return "NL"
	}
	return "CMD"
}

// Result carries the decision plus a human-readable reason for debugging and
// for building the gray-zone training signal later.
type Result struct {
	Label  Label
	Reason string
}

// Classifier holds the (injectable) knowledge of which words name real
// commands. In production this is built from PATH + zsh builtins + aliases; in
// tests it is a fixed curated set so the baseline is reproducible.
type Classifier struct {
	known    map[string]bool // commands/builtins/aliases known to resolve
	grayzone map[string]bool // known commands that are also common English words
}

// New builds a Classifier from a set of known command names. Gray-zone verbs
// are the intersection of `known` with the built-in ambiguous-word list.
func New(known []string) *Classifier {
	c := &Classifier{
		known:    make(map[string]bool, len(known)),
		grayzone: make(map[string]bool),
	}
	for _, k := range known {
		c.known[k] = true
	}
	for w := range grayzoneVerbs {
		if c.known[w] {
			c.grayzone[w] = true
		}
	}
	return c
}

// Classify runs the stage-0 cascade on a single line of input.
func (c *Classifier) Classify(input string) Result {
	s := strings.TrimSpace(input)
	if s == "" {
		return Result{CMD, "empty"}
	}

	// Multi-line input (newline in the trimmed string) is always shell syntax:
	// line continuations (\), for/if/while/case blocks, heredocs, function
	// definitions, etc. Natural language in zsh is always single-line — the
	// widget only intercepts accept-line, and zsh only produces multi-line
	// $BUFFER for incomplete shell constructs.
	if strings.Contains(s, "\n") {
		return Result{CMD, "multi-line"}
	}

	first, structured, parseOK := c.firstWord(s)

	// Structured shell with no ambiguity: assignments, keywords, subshells,
	// pipes/lists, expansions, redirections. Always CMD.
	if structured {
		return Result{CMD, "structured-shell"}
	}

	// A parse failure (unbalanced quotes, stray bytes) is unusual for a real
	// command. Treat it as a weak NL hint but still require an NL signal.
	if !parseOK {
		if hasStrongNLSignal(s) {
			return Result{NL, "parse-fail+nl-signal"}
		}
		return Result{CMD, "parse-fail-lean-cmd"}
	}

	rest := strings.TrimSpace(strings.TrimPrefix(s, first))

	switch {
	case c.grayzone[first]:
		// Ambiguous verb that IS installed. Default CMD; flip to NL only when
		// the arguments clearly read as a sentence.
		if argsLookLikeSentence(rest) {
			return Result{NL, "grayzone-verb+sentence-args"}
		}
		return Result{CMD, "grayzone-verb-cmd"}

	case c.known[first]:
		// Known, non-ambiguous command (git, docker, grep, curl...). Always
		// CMD, even if its quoted args contain English or Chinese.
		return Result{CMD, "known-command"}

	default:
		// Unknown first word.
		if hasStrongNLSignal(s) {
			return Result{NL, "unknown-first+nl-signal"}
		}
		// TitleCase first word + at least one more word: real commands are
		// virtually never TitleCased ("Calculate", "Change", "Archive"...),
		// whereas imperative NL descriptions routinely are. Safe NL signal.
		if rest != "" && isTitleCaseWord(first) {
			return Result{NL, "unknown-first+titlecase"}
		}
		// Multi-word, sentence-like, no command-y tokens -> NL.
		if rest != "" && argsLookLikeSentence(rest) && !hasCommandTokens(s) {
			return Result{NL, "unknown-first+sentence"}
		}
		// Otherwise lean CMD: a single unknown token is more likely a typo or
		// an uninstalled tool than natural language. command_not_found_handle
		// will catch it downstream.
		return Result{CMD, "unknown-lean-cmd"}
	}
}

// firstWord parses the input and returns the literal first word of the first
// simple command. structured is true when the input is non-ambiguous shell
// (assignments, shell keywords, pipes/lists, subshells, or a first word that
// is itself an expansion/quote). parseOK reports whether mvdan/sh accepted it.
func (c *Classifier) firstWord(s string) (first string, structured, parseOK bool) {
	parser := syntax.NewParser(syntax.Variant(syntax.LangBash))
	f, err := parser.Parse(strings.NewReader(s), "")
	if err != nil {
		return "", false, false
	}
	parseOK = true
	if len(f.Stmts) == 0 {
		return "", false, true
	}
	// More than one statement (separated by ; & newline) -> structured.
	if len(f.Stmts) > 1 {
		return "", true, true
	}

	cmd := f.Stmts[0].Cmd
	// Redirections on the statement (>, <, >>) are command syntax.
	if len(f.Stmts[0].Redirs) > 0 {
		return "", true, true
	}

	call, ok := cmd.(*syntax.CallExpr)
	if !ok {
		// for/if/while/case/function/{ }/( )/let/((...)) etc.
		return "", true, true
	}
	// Pure assignment(s) like FOO=bar, or assignments preceding the command.
	if len(call.Assigns) > 0 {
		return "", true, true
	}
	if len(call.Args) == 0 {
		return "", true, true
	}

	w := call.Args[0]
	lit := litValue(w)
	if lit == "" {
		// First word is an expansion, quote, or substitution -> command syntax.
		return "", true, true
	}
	return lit, false, true
}

// litValue returns the plain literal string of a word, or "" if the word
// contains any expansion, quote, or substitution (i.e. it is not a bare word).
func litValue(w *syntax.Word) string {
	if len(w.Parts) != 1 {
		return ""
	}
	lit, ok := w.Parts[0].(*syntax.Lit)
	if !ok {
		return ""
	}
	return lit.Value
}
