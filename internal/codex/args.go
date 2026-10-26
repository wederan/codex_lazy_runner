package codex

import (
	"fmt"
	"strconv"
	"strings"
)

var (
	// Flags `codex exec resume` accepts as-is.
	resumeValueFlags = set("-c", "--config", "--enable", "--disable", "-m", "--model", "--output-schema")
	resumeBoolFlags  = set("--strict-config", "--dangerously-bypass-approvals-and-sandbox",
		"--dangerously-bypass-hook-trust", "--skip-git-repo-check", "--ignore-user-config", "--ignore-rules")
	// Flags that only concern the first run; dropped silently.
	firstRunValueFlags = set("-i", "--image", "-o", "--output-last-message", "--color", "--thread-source")
	firstRunBoolFlags  = set("--json", "--ephemeral")
	// Flags resume does not support; dropped with a warning.
	unsupportedValueFlags = set("-p", "--profile", "--local-provider")
	unsupportedBoolFlags  = set("--worktree", "--oss", "--approve-for-me")
)

func set(names ...string) map[string]bool {
	m := make(map[string]bool, len(names))
	for _, n := range names {
		m[n] = true
	}
	return m
}

// ResumeCompatible converts `codex exec` flags into ones `codex exec resume`
// accepts: -s/--sandbox and --add-dir become -c overrides, and -C/--cd is
// returned as cwd since resume has no such flag.
func ResumeCompatible(args []string) (out []string, cwd string, warnings []string) {
	var addDirs []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		name, value, hasValue := arg, "", false
		if strings.HasPrefix(arg, "--") {
			name, value, hasValue = strings.Cut(arg, "=")
		}
		takeValue := func() (string, bool) {
			if hasValue {
				return value, true
			}
			if i+1 < len(args) {
				i++
				return args[i], true
			}
			return "", false
		}
		switch {
		case !strings.HasPrefix(name, "-"):
			warnings = append(warnings, fmt.Sprintf("忽略多余参数 %q", arg))
		case resumeValueFlags[name]:
			if v, ok := takeValue(); ok {
				out = append(out, name, v)
			}
		case resumeBoolFlags[name]:
			out = append(out, name)
		case name == "-s" || name == "--sandbox":
			if v, ok := takeValue(); ok {
				out = append(out, "-c", "sandbox_mode="+strconv.Quote(v))
			}
		case name == "-C" || name == "--cd":
			cwd, _ = takeValue()
		case name == "--add-dir":
			if v, ok := takeValue(); ok {
				addDirs = append(addDirs, strconv.Quote(v))
			}
		case firstRunValueFlags[name]:
			takeValue()
		case firstRunBoolFlags[name]:
		case unsupportedValueFlags[name]:
			v, _ := takeValue()
			warnings = append(warnings, fmt.Sprintf("codex exec resume 不支持 %s %s，续跑时忽略", name, v))
		case unsupportedBoolFlags[name]:
			warnings = append(warnings, fmt.Sprintf("codex exec resume 不支持 %s，续跑时忽略", name))
		default:
			warnings = append(warnings, fmt.Sprintf("未知参数 %s，续跑时忽略", arg))
		}
	}
	if len(addDirs) > 0 {
		out = append(out, "-c", "sandbox_workspace_write.writable_roots=["+strings.Join(addDirs, ",")+"]")
	}
	return out, cwd, warnings
}
