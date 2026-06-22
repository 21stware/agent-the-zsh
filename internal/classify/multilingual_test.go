package classify

import "testing"

// TestMultilingualCommands verifies that shell commands in various language
// environments (English, Chinese, Japanese, Korean, Spanish, German, French,
// Russian) are correctly classified as CMD.
func TestMultilingualCommands(t *testing.T) {
	c := New(knownCommandsForTest)

	cases := []struct {
		input  string
		reason string
	}{
		// English commands
		{"ls -la", "basic ls"},
		{"git commit -m 'fix bug'", "git commit"},
		{"docker run -d --name web nginx", "docker run"},
		{"find . -name '*.go' -delete", "find delete"},
		{"echo hello world", "echo"},
		{"curl -s https://example.com", "curl"},
		{"ssh user@host", "ssh"},
		{"python3 script.py", "python3"},
		{"npm install --save-dev", "npm install"},

		// Chinese arguments in commands
		{"git commit -m '修复登录bug'", "git commit with Chinese message"},
		{"mkdir 我的文件夹", "mkdir with Chinese name"},
		{"cat 中文文件.txt", "cat with Chinese filename"},
		{"grep '搜索关键词' file.txt", "grep with Chinese search term"},
		{"cp 文件A.txt 文件B.txt", "cp with Chinese filenames"},
		{"echo '这是测试' > output.txt", "echo with Chinese redirect"},

		// Japanese arguments in commands
		{"mkdir フォルダ", "mkdir with Japanese"},
		{"cat 日本語.txt", "cat with Japanese filename"},
		{"git log --oneline -10", "git log"},

		// Korean arguments in commands
		{"mkdir 폴더", "mkdir with Korean"},

		// European language arguments
		{"git commit -m 'Corrige el error de inicio de sesión'", "git commit Spanish"},
		{"git commit -m 'Behebe den Anmeldefehler'", "git commit German"},

		// Russian arguments
		{"mkdir папка", "mkdir with Russian"},

		// Mixed language commands
		{"git commit -m 'Fix 权限问题'", "git commit mixed English-Chinese"},

		// Complex commands with pipes and redirects
		{"cat file.txt | grep 'pattern' | wc -l", "pipe chain"},
		{"echo 'test' >> /tmp/log.txt", "append redirect"},
		{"find . -type f -name '*.py' | xargs grep 'import'", "find + xargs"},
		{"tar -czf archive.tar.gz dir/", "tar create"},

		// Commands with environment variables
		{"FOO=bar baz --flag", "env var prefix"},
		{"PATH=/usr/bin:$PATH mytool", "PATH override"},
	}

	for _, tc := range cases {
		got := c.Classify(tc.input)
		if got.Label != CMD {
			t.Errorf("[%s] %q: got %s, want CMD (reason=%s)", tc.reason, tc.input, got.Label, got.Reason)
		}
	}
}

// TestMultilingualNaturalLanguage verifies that natural language input in
// various languages is correctly classified as NL. The classifier has strong
// signals for English (englishNLSignals) and Chinese (hasChineseNLSignal via
// unicode.Han). Other CJK scripts (Japanese kana, Korean hangul) and
// non-Latin scripts (Cyrillic) are not yet supported as NL signals.
func TestMultilingualNaturalLanguage(t *testing.T) {
	c := New(knownCommandsForTest)

	cases := []struct {
		input  string
		reason string
	}{
		// English NL
		{"how do I fix the failing tests", "English question"},
		{"show me all files larger than 100MB", "English request"},
		{"what is the current git branch", "English question"},
		{"create a new directory called test", "English instruction"},
		{"find all python files and count them", "English request"},

		// Chinese NL (Han characters trigger hasChineseNLSignal)
		{"如何修复失败的测试", "Chinese question"},
		{"显示所有大于100MB的文件", "Chinese request"},
		{"当前git分支是什么", "Chinese question"},
		{"创建一个名为test的新目录", "Chinese instruction"},
		{"查找所有python文件并计数", "Chinese request"},
		{"帮我看看这个项目的结构", "Chinese request"},
		{"为什么这个命令不工作", "Chinese question"},

		// Japanese NL with Kanji (Han characters trigger hasChineseNLSignal)
		{"現在のブランチを教えて", "Japanese with Kanji"},
		{"修正する方法を教えて", "Japanese with Kanji"},

		// European NL with English function words embedded
		{"show me todos los archivos", "Spanish with English signal"},
		{"zeige me alle Dateien", "German with English signal"},
		{"montre me tous les fichiers", "French with English signal"},

		// Mixed NL (Chinese triggers hasChineseNLSignal)
		{"帮我 find all the broken tests", "Mixed Chinese-English"},
		{"show me 这个目录的 files", "Mixed English-Chinese"},
	}

	for _, tc := range cases {
		got := c.Classify(tc.input)
		if got.Label != NL {
			t.Errorf("[%s] %q: got %s, want NL (reason=%s)", tc.reason, tc.input, got.Label, got.Reason)
		}
	}
}

// TestGrayZoneCommands verifies commands that look like natural language
// but are actually shell commands.
func TestGrayZoneCommands(t *testing.T) {
	c := New(knownCommandsForTest)

	cases := []struct {
		input  string
		reason string
	}{
		{"source ~/.zshrc", "source command"},
		{"export PATH=/usr/bin:$PATH", "export command"},
		{"alias ll='ls -la'", "alias command"},
		{"return 0", "return command"},
		{"true && echo success", "true with &&"},
		{"false || echo failed", "false with ||"},
		{"test -f file.txt && echo exists", "test command"},
		{"wait", "wait command"},
		{"history 10", "history command"},
		{"cd /tmp", "cd command"},
		{"cd ..", "cd parent"},
		{"pushd /var", "pushd command"},
		{"popd", "popd command"},
		{"dirs", "dirs command"},
	}

	for _, tc := range cases {
		got := c.Classify(tc.input)
		if got.Label != CMD {
			t.Errorf("[%s] %q: got %s, want CMD (reason=%s)", tc.reason, tc.input, got.Label, got.Reason)
		}
	}
}

// TestEdgeCases tests various edge cases for the classifier.
func TestEdgeCases(t *testing.T) {
	c := New(knownCommandsForTest)

	// Empty input should be CMD (empty).
	if got := c.Classify(""); got.Label != CMD {
		t.Errorf("empty input: got %s, want CMD", got.Label)
	}

	// Whitespace-only input should be CMD.
	if got := c.Classify("   "); got.Label != CMD {
		t.Errorf("whitespace input: got %s, want CMD", got.Label)
	}

	// Single character commands.
	if got := c.Classify("w"); got.Label != CMD {
		t.Errorf("'w': got %s, want CMD (reason=%s)", got.Label, got.Reason)
	}

	// Very long natural language.
	longNL := "can you please help me understand how to properly configure the database connection string for production use with SSL enabled"
	if got := c.Classify(longNL); got.Label != NL {
		t.Errorf("long NL: got %s, want NL (reason=%s)", got.Label, got.Reason)
	}

	// Very long command.
	longCmd := "docker run -d --name myapp -p 8080:80 -e ENV=prod -e DB_HOST=localhost -e DB_PORT=5432 -v /data:/app/data --restart unless-stopped myapp:latest"
	if got := c.Classify(longCmd); got.Label != CMD {
		t.Errorf("long command: got %s, want CMD (reason=%s)", got.Label, got.Reason)
	}
}

// TestCommandsFromDifferentShells verifies commands that use shell-specific
// syntax from bash, zsh, and fish.
func TestCommandsFromDifferentShells(t *testing.T) {
	c := New(knownCommandsForTest)

	cases := []struct {
		input  string
		reason string
	}{
		// Bash/zsh syntax
		{"echo ${VAR:-default}", "bash default var"},
		{"echo $((1+2))", "bash arithmetic"},
		{"echo ${array[@]}", "bash array"},
		{"[[ -f file ]] && echo yes", "bash test"},
		{"echo $RANDOM", "bash random"},
		{"echo !$", "bash history expansion"},

		// Subshells and command substitution
		{"result=$(grep pattern file)", "command substitution"},
		{"(cd /tmp && ls)", "subshell"},
		{"echo $(date +%Y)", "nested command sub"},

		// Process substitution
		{"diff <(sort a) <(sort b)", "process substitution"},
		{"wc -l <(cat file)", "process sub with wc"},

		// Brace expansion
		{"echo {a,b,c}", "brace expansion"},
		{"cp file{,.bak}", "brace backup"},
		{"mkdir -p test/{src,dist,docs}", "brace mkdir"},

		// Here-strings
		{"grep pattern <<< 'text'", "here-string"},
	}

	for _, tc := range cases {
		got := c.Classify(tc.input)
		if got.Label != CMD {
			t.Errorf("[%s] %q: got %s, want CMD (reason=%s)", tc.reason, tc.input, got.Label, got.Reason)
		}
	}
}

// TestQuotedCommands verifies commands with various quoting styles.
func TestQuotedCommands(t *testing.T) {
	c := New(knownCommandsForTest)

	cases := []struct {
		input  string
		reason string
	}{
		{"echo 'single quoted'", "single quotes"},
		{"echo \"double quoted\"", "double quotes"},
		{"git commit -m \"fix: update API endpoint\"", "git commit with quotes"},
		{"sed 's/old/new/g' file.txt", "sed with quotes"},
		{"awk '{print $1}' file.txt", "awk with quotes"},
		{"grep 'pattern' file.txt", "grep with quotes"},
		{"find . -name '*.go' -type f", "find with quotes"},
		{"docker run --name 'my container' nginx", "docker with quotes"},
	}

	for _, tc := range cases {
		got := c.Classify(tc.input)
		if got.Label != CMD {
			t.Errorf("[%s] %q: got %s, want CMD (reason=%s)", tc.reason, tc.input, got.Label, got.Reason)
		}
	}
}
