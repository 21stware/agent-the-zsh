package translate

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/oboo/terflow/internal/llm"
)

func TestClassifyEffect(t *testing.T) {
	cases := []struct {
		cmd  string
		want Effect
	}{
		// read-only
		{"ls -la", EffectReadOnly},
		{"cat file.txt", EffectReadOnly},
		{"grep -r foo .", EffectReadOnly},
		{"find . -name '*.go'", EffectReadOnly},
		{"git status", EffectReadOnly},
		{"git log --oneline", EffectReadOnly},
		{"git diff HEAD~1", EffectReadOnly},
		{"docker ps -a", EffectReadOnly},
		{"ps aux | grep node", EffectReadOnly},
		{"cat access.log | awk '{print $1}' | sort | uniq -c", EffectReadOnly},
		{"sed 's/a/b/' file.txt", EffectReadOnly},
		{"df -h", EffectReadOnly},
		{"echo hello", EffectReadOnly},
		{`find . -name '*.go' -type f`, EffectReadOnly}, // pure traversal

		// side effects
		{"rm -rf build", EffectSideEffect},
		{"mv a b", EffectSideEffect},
		{"cp x y", EffectSideEffect},
		{"git push", EffectSideEffect},
		{"git commit -m x", EffectSideEffect},
		{"git checkout main", EffectSideEffect},
		{"docker rm container", EffectSideEffect},
		{"echo x > file.txt", EffectSideEffect},        // output redirection
		{"cat a >> b", EffectSideEffect},               // append redirection
		{"sed -i 's/a/b/' file.txt", EffectSideEffect}, // in-place
		{"sed -i.bak 's/a/b/' f", EffectSideEffect},
		{"curl -X POST http://x", EffectSideEffect}, // unknown -> caution
		{"mkdir newdir", EffectSideEffect},
		{"chmod +x script.sh", EffectSideEffect},
		{"ls && rm file", EffectSideEffect}, // any unsafe in the list
		{"npm install", EffectSideEffect},
		{"$EDITOR file", EffectSideEffect},                        // dynamic command name
		{"git config user.name x", EffectSideEffect},              // config can write
		{`find . -name '*.o' -delete`, EffectSideEffect},          // -delete mutates
		{`find . -name '*.tmp' -exec rm {} \;`, EffectSideEffect}, // -exec runs a command
		{`find . -type f -execdir chmod 644 {} +`, EffectSideEffect},
		{`find . -name x -fls out.txt`, EffectSideEffect},       // writes a file
		{`fd -e log -x rm`, EffectSideEffect},                   // fd exec
		{`find . -type f -print0 | xargs rm`, EffectSideEffect}, // xargs runs unknown
	}
	for _, c := range cases {
		got := Classify(c.cmd)
		if got != c.want {
			t.Errorf("Classify(%q) = %s, want %s", c.cmd, got, c.want)
		}
	}
}

func TestSanitize(t *testing.T) {
	cases := []struct{ in, want string }{
		{"git status", "git status"},
		{"  git status  ", "git status"},
		{"$ git status", "git status"},
		{"```sh\ngit status\n```", "git status"},
		{"```\nls -la\n```", "ls -la"},
		{"ls -la\n# explanation here", "ls -la"},
		{"# cannot translate", CannotTranslate},
		{"", ""},
	}
	for _, c := range cases {
		if got := sanitize(c.in); got != c.want {
			t.Errorf("sanitize(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

// fixtureServer streams a single text block containing `text` as the command.
func fixtureServer(t *testing.T, text string) *httptest.Server {
	t.Helper()
	// Build a minimal SSE response with one text block.
	body := "event: message_start\n" +
		`data: {"type":"message_start","message":{"model":"claude-haiku-4-5","usage":{"input_tokens":10}}}` + "\n\n" +
		"event: content_block_start\n" +
		`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}` + "\n\n" +
		"event: content_block_delta\n" +
		`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":` + jsonQuote(text) + `}}` + "\n\n" +
		"event: message_delta\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5}}` + "\n\n" +
		"event: message_stop\n" +
		`data: {"type":"message_stop"}` + "\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("content-type", "text/event-stream")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func jsonQuote(s string) string {
	b, _ := json.Marshal(s)
	return string(b)
}

func TestTranslateReadOnly(t *testing.T) {
	srv := fixtureServer(t, "git status")
	c := llm.New("k", llm.WithBaseURL(srv.URL))
	tr := New(c, "")

	var streamed strings.Builder
	res, err := tr.Translate(context.Background(), "show me the git status",
		Context{CWD: "/tmp/proj", History: []string{"cd /tmp/proj"}},
		func(d string) { streamed.WriteString(d) })
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.Untranslatable {
		t.Fatal("unexpected untranslatable")
	}
	if res.Command != "git status" {
		t.Errorf("command = %q, want %q", res.Command, "git status")
	}
	if res.Effect != EffectReadOnly {
		t.Errorf("effect = %s, want read-only", res.Effect)
	}
	if streamed.String() != "git status" {
		t.Errorf("streamed = %q", streamed.String())
	}
}

func TestTranslateSideEffect(t *testing.T) {
	srv := fixtureServer(t, "rm -rf node_modules")
	tr := New(llm.New("k", llm.WithBaseURL(srv.URL)), "")
	res, err := tr.Translate(context.Background(), "delete node_modules", Context{}, nil)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.Effect != EffectSideEffect {
		t.Errorf("effect = %s, want side-effect", res.Effect)
	}
	if res.Command != "rm -rf node_modules" {
		t.Errorf("command = %q", res.Command)
	}
}

func TestTranslateUntranslatable(t *testing.T) {
	srv := fixtureServer(t, "# cannot translate")
	tr := New(llm.New("k", llm.WithBaseURL(srv.URL)), "")
	res, err := tr.Translate(context.Background(), "what is the meaning of life", Context{}, nil)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if !res.Untranslatable {
		t.Errorf("expected untranslatable, got command %q", res.Command)
	}
}

func TestTranslateStripsFence(t *testing.T) {
	srv := fixtureServer(t, "```sh\ndocker ps -a\n```")
	tr := New(llm.New("k", llm.WithBaseURL(srv.URL)), "")
	res, err := tr.Translate(context.Background(), "list all containers", Context{}, nil)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.Command != "docker ps -a" {
		t.Errorf("command = %q, want fenced content stripped", res.Command)
	}
}

func TestTranslateRoutesToAgent(t *testing.T) {
	srv := fixtureServer(t, AgentSentinel)
	tr := New(llm.New("k", llm.WithBaseURL(srv.URL)), "")
	res, err := tr.Translate(context.Background(), "fix all the failing tests", Context{}, nil)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if !res.Agent {
		t.Errorf("expected Agent=true, got command=%q untranslatable=%v", res.Command, res.Untranslatable)
	}
	if res.Command != "" {
		t.Errorf("agent route should have empty command, got %q", res.Command)
	}
}

// TestTranslateProseRoutesToAgent: if the model emits a sentence of explanation
// instead of a command (violating the contract), it must NOT land in the buffer
// — it routes to the agent.
func TestTranslateProseRoutesToAgent(t *testing.T) {
	prose := `I can't translate that. The request "give me IP" is ambiguous—it could mean your local IP or a remote one.`
	srv := fixtureServer(t, prose)
	tr := New(llm.New("k", llm.WithBaseURL(srv.URL)), "")
	res, err := tr.Translate(context.Background(), "give me ip", Context{}, nil)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.Command != "" {
		t.Errorf("prose must not become a command, got %q", res.Command)
	}
	if !res.Agent {
		t.Errorf("prose should route to agent, got untranslatable=%v", res.Untranslatable)
	}
}

func TestLooksLikeProse(t *testing.T) {
	prose := []string{
		`I can't translate that. The request is ambiguous.`,
		`Sorry, could you clarify what you mean?`,
		"这个请求无法翻译，请明确你的意图。",
		`This needs several steps. First read the file, then edit it.`, // sentence boundary ". F"
	}
	notProse := []string{
		"git status",
		"find . -name '*.go'",
		"lsof -i :8080",
		`ifconfig | grep "inet "`,
		"docker ps -a",
		"ls -la /var/log", // has no sentence punctuation
	}
	for _, p := range prose {
		if !looksLikeProse(p) {
			t.Errorf("looksLikeProse(%q) = false, want true", p)
		}
	}
	for _, c := range notProse {
		if looksLikeProse(c) {
			t.Errorf("looksLikeProse(%q) = true, want false (real command)", c)
		}
	}
}
