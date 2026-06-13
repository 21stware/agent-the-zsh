// Package agent implements flow's mode B: a self-built Anthropic tool-use loop
// that can read/write files and run shell commands to complete a multi-step
// task. Safety is enforced not by an explicit entry signal but by a per-tool-
// call permission gate governed by a configurable review level.
package agent

import (
	"strings"

	"github.com/21stware/agent-the-zsh/internal/translate"
)

// ReviewLevel controls how aggressively the permission gate asks for approval.
type ReviewLevel int

const (
	// ReviewStrict: every side-effecting operation must be approved; read-only
	// operations run optimistically.
	ReviewStrict ReviewLevel = iota
	// ReviewFocused (default): only high-risk operations must be approved
	// (rm, git push, sudo, writes to critical paths, output redirections);
	// ordinary writes pass; reads are optimistic.
	ReviewFocused
	// ReviewYolo: nothing is ever asked; everything runs.
	ReviewYolo
)

func (l ReviewLevel) String() string {
	switch l {
	case ReviewStrict:
		return "strict"
	case ReviewYolo:
		return "yolo"
	default:
		return "focused"
	}
}

// ParseReviewLevel maps a config/string value to a ReviewLevel. Unknown values
// fall back to the safe default (focused).
func ParseReviewLevel(s string) ReviewLevel {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "strict":
		return ReviewStrict
	case "yolo", "none", "off":
		return ReviewYolo
	default:
		return ReviewFocused
	}
}

// Risk classifies a tool call's blast radius for the permission gate.
type Risk int

const (
	// RiskReadOnly: only inspects state (read_file, grep, ls/cat/... via bash).
	RiskReadOnly Risk = iota
	// RiskWrite: mutates the filesystem in an ordinary way (write_file, edit, or
	// a side-effecting but not-obviously-dangerous bash command).
	RiskWrite
	// RiskHigh: destructive or wide-blast-radius (rm/rmdir, git push, sudo,
	// writes to paths outside the working tree, force flags, redirections that
	// could clobber, chmod -R, etc.).
	RiskHigh
)

func (r Risk) String() string {
	switch r {
	case RiskReadOnly:
		return "read-only"
	case RiskHigh:
		return "high-risk"
	default:
		return "write"
	}
}

// Decision is the gate's verdict for a tool call.
type Decision int

const (
	// DecideAllow: run without asking.
	DecideAllow Decision = iota
	// DecideAsk: prompt the user to approve/reject.
	DecideAsk
)

// Decide returns whether a tool call of the given risk should run or be asked
// about, under the given review level.
func Decide(level ReviewLevel, risk Risk) Decision {
	if risk == RiskReadOnly {
		return DecideAllow // reads are always optimistic
	}
	switch level {
	case ReviewYolo:
		return DecideAllow
	case ReviewStrict:
		return DecideAsk // any side effect asks
	default: // ReviewFocused
		if risk == RiskHigh {
			return DecideAsk
		}
		return DecideAllow // ordinary writes pass
	}
}

// classifyBash derives a Risk for a bash command. Read-only commands (per the
// shared side-effect classifier) are RiskReadOnly. Otherwise we scan for
// high-risk signatures; anything side-effecting but not matching them is
// RiskWrite.
func classifyBash(command string) Risk {
	if translate.Classify(command) == translate.EffectReadOnly {
		return RiskReadOnly
	}
	if isHighRiskCommand(command) {
		return RiskHigh
	}
	return RiskWrite
}

// highRiskPatterns are substrings/tokens that mark a command as high-risk. This
// is intentionally conservative — when unsure between write and high, we lean
// high so the user is asked.
var highRiskPatterns = []string{
	"rm ", "rm\t", "rmdir", "git push", "git reset --hard", "git clean",
	"sudo ", "doas ", "mkfs", " dd ", "dd ", ":(){", "shutdown", "reboot", "halt",
	"chmod -r", "chown -r", "chmod 777", "truncate",
	"kill -9", "killall", "pkill", "dropdb", "drop database", "drop table",
	"curl ", "wget ", "scp ", "rsync ", // network egress / remote writes
	"npm publish", "pip install", "brew install", "apt install", "apt-get install",
	"--force", "--hard",
}

// isHighRiskCommand reports whether a command line matches a high-risk pattern,
// or redirects output to a path outside the working tree.
func isHighRiskCommand(command string) bool {
	lc := strings.ToLower(command)
	for _, p := range highRiskPatterns {
		if strings.Contains(lc, p) {
			return true
		}
	}
	return writesOutsideTree(command)
}

// writesOutsideTree detects an output redirection ('>' or '>>') whose target is
// an absolute or parent-escaping path. It deliberately ignores stderr-only
// redirects like `2>/dev/null` and `2>&1`, which are harmless and common in
// read-only queries.
func writesOutsideTree(command string) bool {
	b := []byte(command)
	for i := 0; i < len(b); i++ {
		if b[i] != '>' {
			continue
		}
		// Skip `>>` to land on the first '>' only once; both handled the same.
		// Determine the char just before '>' to classify the redirect kind.
		var prev byte
		if i > 0 {
			prev = b[i-1]
		}
		// stderr/fd redirects: `2>`, `&>`, `2>&1`, `1>&2` — and `>&`. The fd
		// digit or '&' before '>' means it's not a plain stdout-to-file write
		// we need to police for path; `2>/dev/null` is harmless.
		if prev == '&' {
			continue
		}
		if prev >= '0' && prev <= '9' {
			// numeric fd like 2> — only police if it's fd 1 (stdout). Treat
			// other fds (2>, etc.) as harmless.
			if prev != '1' {
				continue
			}
		}
		// Find the redirect target (skip '>' and any following '>' and spaces).
		j := i + 1
		for j < len(b) && b[j] == '>' {
			j++
		}
		for j < len(b) && (b[j] == ' ' || b[j] == '\t') {
			j++
		}
		// Read the target token.
		k := j
		for k < len(b) && b[k] != ' ' && b[k] != '\t' && b[k] != '|' && b[k] != ';' && b[k] != '&' {
			k++
		}
		target := string(b[j:k])
		if target == "/dev/null" {
			continue // explicit discard
		}
		if strings.HasPrefix(target, "/") ||
			strings.HasPrefix(target, "../") || strings.Contains(target, "/../") ||
			strings.HasPrefix(target, "~/") {
			return true // writing outside the working tree
		}
	}
	return false
}
