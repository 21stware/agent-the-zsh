// Package session persists a flow agent conversation to a per-shell JSONL
// transcript so a zsh session can behave as one continuous conversation.
//
// Each line is one llm.Message (the Anthropic tagged-union content blocks are
// preserved by llm.ContentBlock's own JSON marshaling), so a stored transcript
// can be replayed to the model verbatim — including tool_use / tool_result
// turns. Storage is always-on; loading is opt-in (the agent only seeds history
// when asked to resume).
//
// The transcript lives under the same per-uid directory as the daemon socket
// (see daemon.SocketPath), in a sessions/<id>.jsonl file, 0700 dir / 0600 file —
// matching the socket's secrecy, since transcripts can contain command output
// and file contents.
package session

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/21stware/agent-the-zsh/internal/llm"
)

// Path returns the JSONL transcript path for the given session id, creating the
// sessions directory (0700) if needed. The base mirrors daemon.SocketPath:
//
//	$XDG_RUNTIME_DIR/flow/sessions/<id>.jsonl
//	else $TMPDIR/flow-<uid>/sessions/<id>.jsonl   (macOS has no XDG_RUNTIME_DIR)
//
// An empty id returns ("", nil): persistence is disabled and every call here
// becomes a clean no-op (e.g. an agent run outside a flow-wired shell).
func Path(id string) (string, error) {
	if id == "" {
		return "", nil
	}
	dir, err := Dir()
	if err != nil {
		return "", err
	}
	return filepath.Join(dir, id+".jsonl"), nil
}

// Dir returns the sessions directory (creating it 0700), mirroring the base
// resolution of daemon.SocketPath: $XDG_RUNTIME_DIR/flow/sessions else
// $TMPDIR/flow-<uid>/sessions.
func Dir() (string, error) {
	var base string
	if rt := os.Getenv("XDG_RUNTIME_DIR"); rt != "" {
		base = filepath.Join(rt, "flow")
	} else {
		tmp := os.Getenv("TMPDIR")
		if tmp == "" {
			tmp = "/tmp"
		}
		base = filepath.Join(tmp, fmt.Sprintf("flow-%d", os.Getuid()))
	}
	dir := filepath.Join(base, "sessions")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", err
	}
	return dir, nil
}

// Append appends each message as one JSON line to path. The file is opened
// O_APPEND|O_CREATE|O_WRONLY (0600); each message is written with a single
// Write, so O_APPEND keeps lines intact even if another run in the same session
// interleaves. A no-op when path is empty (persistence disabled).
func Append(path string, msgs []llm.Message) error {
	if path == "" || len(msgs) == 0 {
		return nil
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer f.Close()
	for _, m := range msgs {
		b, err := json.Marshal(m)
		if err != nil {
			return err
		}
		b = append(b, '\n')
		if _, err := f.Write(b); err != nil {
			return err
		}
	}
	return nil
}

// Load reads all newline-delimited llm.Message records from path. It returns
// (nil, nil) when the file does not exist (a fresh session). Unparseable lines
// are skipped, so a truncated final line from a crash mid-write does not lose
// the valid history before it.
func Load(path string) ([]llm.Message, error) {
	if path == "" {
		return nil, nil
	}
	f, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	defer f.Close()

	var msgs []llm.Message
	sc := bufio.NewScanner(f)
	// Transcripts can carry large tool outputs; raise the line cap well above
	// the 64K default.
	sc.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var m llm.Message
		if err := json.Unmarshal(line, &m); err != nil {
			continue // skip a partial/corrupt line, keep the rest
		}
		msgs = append(msgs, m)
	}
	if err := sc.Err(); err != nil {
		return msgs, err
	}
	return msgs, nil
}

// SaveMeta records the working directory for a session id in a sidecar
// <id>.meta file (JSON), keeping the .jsonl a pure message stream. Best-effort;
// a no-op when id is empty. Only writes once per session (first turn) to record
// where the conversation started.
func SaveMeta(id, cwd string) error {
	if id == "" {
		return nil
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	mp := filepath.Join(dir, id+".meta")
	if _, err := os.Stat(mp); err == nil {
		return nil // already recorded
	}
	b, err := json.Marshal(meta{Cwd: cwd})
	if err != nil {
		return err
	}
	return os.WriteFile(mp, b, 0o600)
}

type meta struct {
	Cwd string `json:"cwd"`
}

// SaveLevel persists the review level for a session id in a sidecar
// <id>.level file. This lets the "allow all" (yolo) choice survive across
// separate flow-agent invocations within the same shell session. Best-effort;
// a no-op when id is empty.
func SaveLevel(id, level string) error {
	if id == "" {
		return nil
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(dir, id+".level"), []byte(level), 0o600)
}

// LoadLevel reads the persisted review level for a session id. Returns "" if
// no level file exists (the env/default level should be used).
func LoadLevel(id string) string {
	if id == "" {
		return ""
	}
	dir, err := Dir()
	if err != nil {
		return ""
	}
	b, err := os.ReadFile(filepath.Join(dir, id+".level"))
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// ClearLevel removes the persisted review level for a session id, so a
// flowclear resets the "allow all" state. Best-effort; a no-op when id is
// empty or the file does not exist.
func ClearLevel(id string) error {
	if id == "" {
		return nil
	}
	dir, err := Dir()
	if err != nil {
		return err
	}
	return os.Remove(filepath.Join(dir, id+".level"))
}

// Info summarizes one stored session for the resume picker.
type Info struct {
	ID       string // session id (file stem)
	Cwd      string // working directory the conversation started in ("" if unknown)
	LastUser string // text of the most recent user turn ("" if none)
	Turns    int    // message count in the transcript
	ModTime  int64  // transcript mtime (unix seconds), newest first when sorted
}

// List enumerates stored sessions (newest first by transcript mtime), excluding
// excludeID. Empty/credential-less transcripts and the current session are
// skipped by the caller via excludeID. Sessions with no user turn are omitted.
func List(excludeID string) ([]Info, error) {
	dir, err := Dir()
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []Info
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".jsonl") {
			continue
		}
		id := strings.TrimSuffix(name, ".jsonl")
		if id == excludeID {
			continue
		}
		p := filepath.Join(dir, name)
		msgs, _ := Load(p)
		if len(msgs) == 0 {
			continue
		}
		last := lastUserText(msgs)
		if last == "" {
			continue
		}
		var mt int64
		if fi, err := e.Info(); err == nil {
			mt = fi.ModTime().Unix()
		}
		cwd := ""
		if b, err := os.ReadFile(filepath.Join(dir, id+".meta")); err == nil {
			var m meta
			if json.Unmarshal(b, &m) == nil {
				cwd = m.Cwd
			}
		}
		out = append(out, Info{ID: id, Cwd: cwd, LastUser: last, Turns: len(msgs), ModTime: mt})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ModTime > out[j].ModTime })
	return out, nil
}

// lastUserText returns the text of the last user-role message's first text block.
func lastUserText(msgs []llm.Message) string {
	for i := len(msgs) - 1; i >= 0; i-- {
		if msgs[i].Role != llm.RoleUser {
			continue
		}
		for _, b := range msgs[i].Content {
			if b.Type == "text" && b.Text != "" {
				return b.Text
			}
		}
	}
	return ""
}
