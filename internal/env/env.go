// Package env discovers which command names resolve on this machine, so the
// classifier's "known command" set reflects reality instead of a hardcoded
// list. This runs once at daemon startup (and can be refreshed), never on the
// hot path of a single classification.
package env

import (
	"os"
	"path/filepath"
	"strings"
)

// zshBuiltins are common zsh/sh builtins and reserved words. These never appear
// in PATH but must be treated as known commands. Kept conservative; the cost of
// a missing builtin is only a possible NL misroute (safe direction).
var zshBuiltins = []string{
	// POSIX/sh builtins
	"cd", "pwd", "echo", "printf", "read", "test", "export", "unset", "set",
	"shift", "eval", "exec", "exit", "return", "trap", "wait", "umask", "ulimit",
	"alias", "unalias", "type", "hash", "command", "getopts", "times", "true",
	"false", "kill", "jobs", "bg", "fg", "wait", "let", "local", "readonly",
	// zsh-specific builtins / commands
	"setopt", "unsetopt", "autoload", "zstyle", "bindkey", "zle", "compdef",
	"zmodload", "whence", "where", "which", "typeset", "declare", "float",
	"integer", "print", "pushd", "popd", "dirs", "disown", "fc", "history",
	"source", "emulate", "noglob", "nocorrect", "sched", "vared", "zcompile",
	// shell reserved words (parsed structurally, but harmless to include)
	"if", "then", "else", "elif", "fi", "for", "while", "until", "do", "done",
	"case", "esac", "select", "function", "time", "coproc",
}

// Known returns the set of command names available on this machine: every
// executable found on each PATH entry, plus the builtin list. Aliases are
// supplied separately by the client (the daemon cannot see the user's
// interactive alias table), via AddAliases.
func Known() map[string]bool {
	known := make(map[string]bool, 4096)
	for _, b := range zshBuiltins {
		known[b] = true
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			continue
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue // unreadable PATH entry, skip
		}
		for _, e := range entries {
			name := e.Name()
			if name == "" || strings.ContainsAny(name, "/") {
				continue
			}
			// Accept regular files and symlinks; trust the +x convention loosely.
			// We don't stat every file for the x bit: being too strict risks
			// dropping a real command (unsafe NL misroute); being loose only
			// risks treating a non-exec file as a command (still gets accept-line
			// and fails harmlessly downstream).
			known[name] = true
		}
	}
	return known
}

// AddAliases merges client-reported alias names into a known set in place.
func AddAliases(known map[string]bool, aliases []string) {
	for _, a := range aliases {
		a = strings.TrimSpace(a)
		if a != "" {
			known[a] = true
		}
	}
}

// Keys returns the known set as a slice, for passing to classify.New.
func Keys(known map[string]bool) []string {
	out := make([]string, 0, len(known))
	for k := range known {
		out = append(out, k)
	}
	return out
}
