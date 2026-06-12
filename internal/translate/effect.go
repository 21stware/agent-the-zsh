package translate

import (
	"strings"

	"mvdan.cc/sh/v3/syntax"
)

// Effect classifies a command's blast radius. The design rule (constraint 6):
// read-only commands may be handled optimistically, anything with side effects
// must be flagged so the widget can require an explicit confirmation keystroke.
type Effect int

const (
	// EffectReadOnly: the command only reads/inspects (ls, cat, grep, git status…).
	EffectReadOnly Effect = iota
	// EffectSideEffect: writes, deletes, moves, network mutations, or anything
	// not provably read-only. This is the conservative default.
	EffectSideEffect
)

func (e Effect) String() string {
	if e == EffectReadOnly {
		return "read-only"
	}
	return "side-effect"
}

// readOnlyCommands are commands whose normal use only inspects state. A command
// is read-only only if EVERY command name in it is on this list AND it has no
// output redirection. Bias is to caution: unknown commands -> side-effect.
var readOnlyCommands = map[string]bool{
	"ls": true, "ll": true, "la": true, "l": true, "cat": true, "bat": true,
	"less": true, "more": true, "head": true, "tail": true, "tac": true,
	"grep": true, "egrep": true, "fgrep": true, "rg": true, "ag": true,
	"find": true, "fd": true, "locate": true, "which": true, "type": true,
	"whereis": true, "file": true, "stat": true, "wc": true, "sort": true,
	"uniq": true, "cut": true, "tr": true, "awk": true,
	"echo": true, "printf": true, "pwd": true, "whoami": true, "id": true,
	"date": true, "cal": true, "uptime": true, "df": true, "du": true,
	"free": true, "ps": true, "top": true, "htop": true, "env": true,
	"printenv": true, "history": true, "tree": true, "exa": true, "eza": true,
	"diff": true, "cmp": true, "md5sum": true, "sha256sum": true, "cksum": true,
	"hexdump": true, "xxd": true, "od": true, "strings": true, "nl": true,
	"column": true, "jq": true, "yq": true, "basename": true, "dirname": true,
	"realpath": true, "readlink": true, "test": true, "true": true, "false": true,
	"man": true, "tldr": true, "info": true, "help": true, "uname": true,
	"hostname": true, "groups": true, "users": true, "who": true, "w": true,
	"lsof": true, "netstat": true, "ss": true, "ip": true, "ifconfig": true,
	"ping": true, "dig": true, "nslookup": true, "host": true,
}

// gitReadOnlySubcommands are git subcommands that don't mutate the repo or remote.
var gitReadOnlySubcommands = map[string]bool{
	"status": true, "log": true, "diff": true, "show": true, "branch": true,
	"remote": true, "config": false, // config can write; be cautious
	"describe": true, "blame": true, "shortlog": true, "reflog": true,
	"ls-files": true, "ls-remote": true, "rev-parse": true, "cat-file": true,
	"tag":   false, // tag can create; cautious
	"stash": false, "fetch": true, "whatchanged": true, "grep": true,
}

// dockerReadOnlySubcommands: docker subcommands that only inspect.
var dockerReadOnlySubcommands = map[string]bool{
	"ps": true, "images": true, "logs": true, "inspect": true, "version": true,
	"info": true, "stats": true, "top": true, "port": true, "history": true,
	"diff": true, "search": true,
}

// Classify returns the Effect of a single command line. It is conservative: any
// parse failure, redirection, unknown command, or known-mutating command yields
// EffectSideEffect.
func Classify(cmd string) Effect {
	parser := syntax.NewParser()
	f, err := parser.Parse(strings.NewReader(cmd), "")
	if err != nil {
		return EffectSideEffect // can't prove safe -> caution
	}

	safe := true
	syntax.Walk(f, func(node syntax.Node) bool {
		if !safe {
			return false
		}
		switch n := node.(type) {
		case *syntax.Redirect:
			// Output redirections (>, >>, etc.) write files. Input (<) is fine,
			// but distinguishing is fiddly; any redirect -> caution except a
			// pure here-string/input. Be conservative: treat > and >> as unsafe.
			if n.Op == syntax.RdrOut || n.Op == syntax.AppOut ||
				n.Op == syntax.RdrAll || n.Op == syntax.AppAll ||
				n.Op == syntax.ClbOut {
				safe = false
				return false
			}
		case *syntax.CallExpr:
			if !callIsReadOnly(n) {
				safe = false
				return false
			}
		}
		return true
	})

	if safe {
		return EffectReadOnly
	}
	return EffectSideEffect
}

// callIsReadOnly checks a single simple command (first word + args).
func callIsReadOnly(call *syntax.CallExpr) bool {
	// Assignments-only (FOO=bar) with no command: treat as side-effect-free but
	// pointless; allow.
	if len(call.Args) == 0 {
		return true
	}
	name := litWord(call.Args[0])
	if name == "" {
		return false // dynamic command name -> caution
	}

	switch name {
	case "git":
		return subcommandSafe(call, gitReadOnlySubcommands)
	case "docker", "podman":
		return subcommandSafe(call, dockerReadOnlySubcommands)
	case "sed":
		// sed is read-only unless -i (in-place) is present.
		return !hasFlag(call, "-i") && !hasInPlaceSed(call)
	}

	ro, known := readOnlyCommands[name]
	return known && ro
}

// subcommandSafe checks the second word of a subcommand-style tool against an
// allowlist. Unknown subcommands -> not safe.
func subcommandSafe(call *syntax.CallExpr, allow map[string]bool) bool {
	// find the first non-flag argument after the command name
	for _, a := range call.Args[1:] {
		w := litWord(a)
		if w == "" {
			return false
		}
		if strings.HasPrefix(w, "-") {
			continue // global flag like `git -C path`
		}
		ro, known := allow[w]
		return known && ro
	}
	return false
}

func hasFlag(call *syntax.CallExpr, flag string) bool {
	for _, a := range call.Args[1:] {
		if litWord(a) == flag {
			return true
		}
	}
	return false
}

// hasInPlaceSed catches `sed -i...` where -i is combined (e.g. -i.bak or -ie).
func hasInPlaceSed(call *syntax.CallExpr) bool {
	for _, a := range call.Args[1:] {
		w := litWord(a)
		if strings.HasPrefix(w, "-i") {
			return true
		}
	}
	return false
}

// litWord returns the literal string of a word, or "" if it contains any
// expansion/quote/substitution.
func litWord(w *syntax.Word) string {
	if w == nil || len(w.Parts) != 1 {
		// a quoted literal "ls" is a single SglQuoted/DblQuoted part; handle the
		// common bare-literal case and let everything else be "unknown".
		if w != nil && len(w.Parts) >= 1 {
			if lit, ok := w.Parts[0].(*syntax.Lit); ok && len(w.Parts) == 1 {
				return lit.Value
			}
		}
		return ""
	}
	lit, ok := w.Parts[0].(*syntax.Lit)
	if !ok {
		return ""
	}
	return lit.Value
}
