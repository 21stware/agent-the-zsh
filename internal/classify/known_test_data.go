package classify

// knownCommandsForTest is a curated set of command names treated as "installed"
// for reproducible accuracy testing. In production this is generated at daemon
// startup from PATH + zsh builtins + aliases. It deliberately includes the
// gray-zone verbs so the gray-zone path is exercised.
var knownCommandsForTest = []string{
	// core coreutils / common binaries
	"ls", "cat", "grep", "egrep", "fgrep", "rg", "find", "fd", "sed", "awk",
	"cut", "tr", "sort", "uniq", "head", "tail", "wc", "tee", "xargs", "tac",
	"cp", "mv", "rm", "mkdir", "rmdir", "touch", "ln", "ln", "chmod", "chown",
	"chgrp", "stat", "file", "du", "df", "ps", "top", "htop", "kill", "killall",
	"pkill", "pgrep", "jobs", "bg", "fg", "nice", "renice", "nohup", "time",
	"watch", "sleep", "date", "cal", "uptime", "who", "whoami", "users", "id",
	"echo", "printf", "yes", "true", "false", "test", "seq", "expr", "bc",
	"env", "export", "set", "unset", "alias", "which", "type", "command",
	"man", "tldr", "info", "help", "history", "clear", "reset", "tput",
	// editors / pagers
	"vim", "vi", "nvim", "nano", "emacs", "less", "more", "view", "code",
	// vcs / dev
	"git", "gh", "glab", "svn", "hg", "make", "cmake", "ninja", "gcc", "g++",
	"clang", "go", "cargo", "rustc", "node", "npm", "npx", "pnpm", "yarn",
	"python", "python3", "pip", "pip3", "ruby", "gem", "bundle", "java",
	"javac", "mvn", "gradle", "dotnet", "php", "perl", "lua",
	// networking / transfer
	"curl", "wget", "ssh", "scp", "sftp", "rsync", "nc", "netcat", "ping",
	"traceroute", "dig", "nslookup", "host", "ip", "ifconfig", "netstat",
	"ss", "lsof", "telnet", "openssl",
	// containers / cloud / infra
	"docker", "docker-compose", "podman", "kubectl", "k", "helm", "terraform",
	"aws", "gcloud", "az", "vagrant", "ansible",
	// archives / data
	"tar", "gzip", "gunzip", "zip", "unzip", "bzip2", "xz", "zstd", "7z",
	"jq", "yq", "sqlite3", "psql", "mysql", "redis-cli", "mongo",
	// misc tools
	"tmux", "screen", "fzf", "bat", "exa", "eza", "tree", "ncdu", "btop",
	"ffmpeg", "convert", "magick", "pandoc", "brew", "apt", "apt-get", "dnf",
	"pacman", "yum", "snap", "open", "say", "pbcopy", "pbpaste", "cd", "pushd",
	"popd", "dirs", "pwd", "z", "zoxide", "g", "ll", "la", "l",
	// gray-zone-ish that are real commands
	"split", "fold", "expand", "fmt", "join", "look", "last", "at", "link",
	"wait", "list", "show", "run", "pull", "push", "build", "start", "stop",
	"clean", "install", "remove", "see",
}
