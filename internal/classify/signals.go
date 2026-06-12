package classify

import (
	"strings"
	"unicode"
)

// grayzoneVerbs are command names that are also common English words. When one
// of these is installed AND appears as the first word, we cannot decide from
// the first word alone; we inspect the argument portion. This set is the
// universe of candidates; New() keeps only those actually present on the box.
var grayzoneVerbs = map[string]bool{
	"make": true, "find": true, "time": true, "kill": true, "test": true,
	"sort": true, "touch": true, "yes": true, "echo": true, "head": true,
	"tail": true, "cut": true, "join": true, "look": true, "watch": true,
	"see": true, "open": true, "say": true, "tree": true, "history": true,
	"jobs": true, "which": true, "type": true, "help": true, "info": true,
	"date": true, "cal": true, "at": true, "split": true, "expand": true,
	"fold": true, "fmt": true, "last": true, "users": true, "who": true,
	"link": true, "wait": true, "true": true, "false": true, "uniq": true,
	"more": true, "less": true, "file": true, "stat": true, "list": true,
	"show": true, "run": true, "pull": true, "push": true, "build": true,
	"start": true, "stop": true, "clean": true, "install": true, "remove": true,
}

// englishNLSignals are tokens that strongly indicate an English natural-language
// request rather than a command. Presence of any (as a whole word) is a strong
// NL signal. Kept deliberately tight to avoid false NL positives.
var englishNLSignals = map[string]bool{
	"please": true, "how": true, "what": true, "why": true, "where": true,
	"when": true, "which": true, "who": true, "can": true, "could": true,
	"would": true, "should": true, "my": true, "me": true, "the": true,
	"all": true, "every": true, "into": true, "from": true, "with": true,
	"that": true, "this": true, "these": true, "those": true, "their": true,
	"whether": true, "about": true, "out": true, "some": true, "any": true,
	"want": true, "need": true, "help": true, "let": true, "give": true,
	"tell": true, "convert": true, "rename": true, "delete": true,
}

// englishStopVerbsAfter are verbs/words that, when they FOLLOW a gray-zone verb,
// suggest a sentence ("make THE project", "find MY file", "kill THE process").
// These are function words rare in real command arguments.
var englishFunctionWords = map[string]bool{
	"the": true, "a": true, "an": true, "my": true, "your": true, "his": true,
	"her": true, "their": true, "our": true, "this": true, "that": true,
	"these": true, "those": true, "all": true, "every": true, "some": true,
	"any": true, "out": true, "how": true, "what": true, "whether": true,
	"which": true, "where": true, "when": true, "why": true, "me": true,
	"us": true, "them": true, "into": true, "about": true, "faster": true,
	"longer": true,
}

// hasStrongNLSignal reports whether the line contains a strong NL signal:
// a Chinese NL marker, or an English NL signal word OUTSIDE of any quotes.
// Text inside quotes is treated as command argument data, not NL intent
// (e.g. git commit -m "fix the bug please" is CMD).
func hasStrongNLSignal(s string) bool {
	if hasChineseNLSignal(s) {
		return true
	}
	for _, tok := range tokensOutsideQuotes(s) {
		if englishNLSignals[strings.ToLower(tok)] {
			return true
		}
	}
	return false
}

// hasChineseNLSignal reports whether the line contains CJK characters together
// with a Chinese request marker. Any CJK in the line is itself a strong NL
// signal in practice (shell commands are ASCII), but we still gate on markers
// or bare CJK to stay conservative.
func hasChineseNLSignal(s string) bool {
	hasCJK := false
	for _, r := range s {
		if unicode.Is(unicode.Han, r) {
			hasCJK = true
			break
		}
	}
	if !hasCJK {
		return false
	}
	// CJK present. A line containing Han characters is natural language for our
	// purposes regardless of whether it also names a command (e.g. "帮我 grep
	// 一下"), because no real shell command line is written in Chinese.
	return true
}

// chineseMarkers kept for documentation / potential future weighting.
var chineseMarkers = []string{
	"把", "帮我", "帮", "请", "列出", "显示", "查找", "查询", "删除", "看看",
	"能不能", "能否", "怎么", "如何", "所有", "一下", "用", "让", "给我",
}

// argsLookLikeSentence reports whether the argument portion (everything after
// the first word) reads like a natural-language sentence rather than command
// arguments. Heuristic, tuned to lean CMD:
//   - empty args -> not a sentence (bare command).
//   - any flag-like (-x, --long), path-like (/, ./, ~), glob, or option token
//     -> command args, not a sentence.
//   - presence of an English function word among the args -> sentence.
//   - 3+ plain alpha words with no command-y tokens -> sentence.
func argsLookLikeSentence(rest string) bool {
	rest = strings.TrimSpace(rest)
	if rest == "" {
		return false
	}
	if hasChineseNLSignal(rest) {
		return true
	}
	toks := tokensOutsideQuotes(rest)
	if len(toks) == 0 {
		// All args were quoted -> treat as command data, not a sentence.
		return false
	}
	plainWords := 0
	for _, t := range toks {
		if looksCommandy(t) {
			return false
		}
		lt := strings.ToLower(t)
		if englishFunctionWords[lt] {
			return true
		}
		if isPlainWord(t) {
			plainWords++
		}
	}
	return plainWords >= 3
}

// hasCommandTokens reports whether the line contains tokens that are
// characteristic of shell commands (flags, paths, globs, operators) outside of
// quotes. Used to keep unknown-first-word sentence detection from firing on
// things that are clearly command-shaped.
func hasCommandTokens(s string) bool {
	for _, t := range tokensOutsideQuotes(s) {
		if looksCommandy(t) {
			return true
		}
	}
	return false
}

// looksCommandy reports whether a single token looks like a command argument
// (flag, path, glob, env-ish, or contains shell metacharacters).
func looksCommandy(t string) bool {
	if t == "" {
		return false
	}
	if strings.HasPrefix(t, "-") { // -x, --long
		return true
	}
	if strings.ContainsAny(t, "/\\*?[]{}=$|<>~") {
		return true
	}
	if strings.HasPrefix(t, ".") && t != "." && t != ".." {
		return true // .hidden, ./x handled by '/' above
	}
	// dotted filename like foo.txt, a.out
	if i := strings.LastIndex(t, "."); i > 0 && i < len(t)-1 {
		ext := t[i+1:]
		if isAllLetterDigit(ext) && len(ext) <= 5 {
			return true
		}
	}
	return false
}

// isPlainWord reports whether a token is a plain alphabetic word (English-ish),
// the kind that makes up a sentence.
func isPlainWord(t string) bool {
	if t == "" {
		return false
	}
	for _, r := range t {
		if !unicode.IsLetter(r) {
			return false
		}
	}
	return true
}

func isAllLetterDigit(s string) bool {
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			return false
		}
	}
	return s != ""
}

// isTitleCaseWord reports whether a token is an alphabetic word with an
// uppercase first letter and at least one lowercase letter ("Calculate",
// "Change"). All-caps tokens (env var names like RBENV, or acronyms) are
// excluded — those appear in real shell usage.
func isTitleCaseWord(t string) bool {
	if len([]rune(t)) < 2 {
		return false
	}
	rs := []rune(t)
	if !unicode.IsUpper(rs[0]) {
		return false
	}
	hasLower := false
	for _, r := range rs[1:] {
		if !unicode.IsLetter(r) {
			return false
		}
		if unicode.IsLower(r) {
			hasLower = true
		}
	}
	return hasLower
}

// tokensOutsideQuotes splits on whitespace but drops anything inside single or
// double quotes, so quoted NL inside a real command does not count as NL.
func tokensOutsideQuotes(s string) []string {
	var toks []string
	var b strings.Builder
	var quote rune
	flush := func() {
		if b.Len() > 0 {
			toks = append(toks, b.String())
			b.Reset()
		}
	}
	for _, r := range s {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				flush() // end of quoted span; its contents are discarded
			}
			// else: inside quotes, skip
		case r == '\'' || r == '"':
			flush()
			quote = r
		case unicode.IsSpace(r):
			flush()
		default:
			b.WriteRune(r)
		}
	}
	flush()
	return toks
}
