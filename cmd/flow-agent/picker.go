package main

import (
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/21stware/agent-the-zsh/internal/session"
)

// runResumePicker draws an arrow-key menu of stored sessions and prints the
// chosen session id to stdout. The menu is drawn on the controlling TTY
// (/dev/tty) so stdout stays clean for the caller (`flowrsm`). Prints nothing
// and exits 1 if there's nothing to pick or the user cancels.
func runResumePicker() {
	exclude := os.Getenv("FLOW_SESSION_ID")
	infos, err := session.List(exclude)
	if err != nil || len(infos) == 0 {
		fmt.Fprintln(os.Stderr, cDim+"flow: no other sessions to resume."+cReset)
		os.Exit(1)
	}

	tty, err := os.OpenFile("/dev/tty", os.O_RDWR, 0)
	if err != nil {
		// No TTY (piped): fall back to the most recent session.
		fmt.Println(infos[0].ID)
		return
	}
	defer tty.Close()

	// Enter raw mode so we get arrow keys without line buffering / echo.
	restore, err := makeRaw(tty)
	if err != nil {
		fmt.Println(infos[0].ID) // can't do interactive; pick newest
		return
	}
	defer restore()

	sel := 0
	draw := func() { renderMenu(tty, infos, sel) }
	draw()

	buf := make([]byte, 3)
	for {
		n, err := tty.Read(buf)
		if err != nil || n == 0 {
			clearMenu(tty, len(infos))
			os.Exit(1)
		}
		switch {
		case buf[0] == 3 || buf[0] == 'q' || buf[0] == 27 && n == 1: // Ctrl-C, q, bare Esc
			clearMenu(tty, len(infos))
			os.Exit(1)
		case buf[0] == '\r' || buf[0] == '\n': // Enter
			clearMenu(tty, len(infos))
			fmt.Println(infos[sel].ID)
			return
		case n == 3 && buf[0] == 27 && buf[1] == '[':
			switch buf[2] {
			case 'A': // up
				if sel > 0 {
					sel--
				}
			case 'B': // down
				if sel < len(infos)-1 {
					sel++
				}
			}
			draw()
		case buf[0] == 'k': // vim up
			if sel > 0 {
				sel--
			}
			draw()
		case buf[0] == 'j': // vim down
			if sel < len(infos)-1 {
				sel++
			}
			draw()
		}
	}
}

// renderMenu draws the session list, highlighting the selected row. Each row is
// "dir — last message". Drawn from the top; the cursor is moved back up after.
func renderMenu(tty *os.File, infos []session.Info, sel int) {
	var b strings.Builder
	fmt.Fprintf(&b, "\r%s flow · resume which conversation?%s  %s↑/↓ then Enter · Esc cancels%s\n",
		cBold+cCyan, cReset, cDim, cReset)
	for i, in := range infos {
		dir := shortenDir(in.Cwd)
		msg := oneLine(in.LastUser, 56)
		marker, style := "  ", cReset
		if i == sel {
			marker, style = cCyan+"❯ "+cReset, cBold
		}
		// dir dimmed, last message in the row style.
		fmt.Fprintf(&b, "\r%s%s%s%s  %s%s\n",
			marker, cDim, dir, cReset, style, msg+cReset)
	}
	// Move the cursor back to the top of the menu for the next redraw.
	fmt.Fprintf(&b, "\033[%dA", len(infos)+1)
	_, _ = tty.WriteString(b.String())
}

// clearMenu erases the menu (header + one line per entry) and returns the
// cursor to the start of the line.
func clearMenu(tty *os.File, n int) {
	var b strings.Builder
	for i := 0; i < n+1; i++ {
		b.WriteString("\r\033[K\n")
	}
	fmt.Fprintf(&b, "\033[%dA\r", n+1)
	_, _ = tty.WriteString(b.String())
}

// shortenDir replaces $HOME with ~ and shows "(unknown dir)" for empty cwd.
func shortenDir(dir string) string {
	if dir == "" {
		return "(unknown dir)"
	}
	if home, err := os.UserHomeDir(); err == nil && home != "" {
		if dir == home {
			return "~"
		}
		if strings.HasPrefix(dir, home+"/") {
			return "~" + dir[len(home):]
		}
	}
	return dir
}

// oneLine collapses whitespace and truncates to max runes with an ellipsis.
func oneLine(s string, max int) string {
	s = strings.Join(strings.Fields(s), " ")
	r := []rune(s)
	if len(r) > max {
		return string(r[:max-1]) + "…"
	}
	return s
}

// makeRaw puts the tty into raw mode using stty (no cgo, no extra deps) and
// returns a restore func. flow is a shell tool, so stty is a safe assumption.
func makeRaw(tty *os.File) (func(), error) {
	saved, err := sttyState(tty)
	if err != nil {
		return nil, err
	}
	if err := stty(tty, "-echo", "-icanon", "min", "1", "time", "0"); err != nil {
		return nil, err
	}
	// Hide the cursor during the menu.
	_, _ = tty.WriteString("\033[?25l")
	return func() {
		_, _ = tty.WriteString("\033[?25h")
		_ = stty(tty, saved)
	}, nil
}

func sttyState(tty *os.File) (string, error) {
	cmd := exec.Command("stty", "-g")
	cmd.Stdin = tty
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func stty(tty *os.File, args ...string) error {
	cmd := exec.Command("stty", args...)
	cmd.Stdin = tty
	return cmd.Run()
}
