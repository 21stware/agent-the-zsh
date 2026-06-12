package translate

import "testing"

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
