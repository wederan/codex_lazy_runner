package codex

import (
	"reflect"
	"testing"
)

func TestResumeCompatible(t *testing.T) {
	out, cwd, warnings := ResumeCompatible([]string{
		"-m", "gpt-5.5", "-c", `model_reasoning_effort="high"`, "--json",
		"-s", "workspace-write", "-C", "/work", "--add-dir", "/a", "--add-dir=/b",
		"-i", "shot.png", "--skip-git-repo-check", "--profile", "fast", "--bogus",
	})
	want := []string{
		"-m", "gpt-5.5", "-c", `model_reasoning_effort="high"`,
		"-c", `sandbox_mode="workspace-write"`, "--skip-git-repo-check",
		"-c", `sandbox_workspace_write.writable_roots=["/a","/b"]`,
	}
	if !reflect.DeepEqual(out, want) {
		t.Errorf("out =\n%q\nwant\n%q", out, want)
	}
	if cwd != "/work" {
		t.Errorf("cwd = %q", cwd)
	}
	if len(warnings) != 2 {
		t.Errorf("warnings = %q", warnings)
	}
}

func TestArgs(t *testing.T) {
	if got := ExecArgs("-fix it", []string{"-m", "x"}); !reflect.DeepEqual(got, []string{"exec", "--json", "-m", "x", "--", "-fix it"}) {
		t.Errorf("ExecArgs = %q", got)
	}
	if got := ResumeArgs("id", "go on", nil); !reflect.DeepEqual(got, []string{"exec", "resume", "--json", "--skip-git-repo-check", "--", "id", "go on"}) {
		t.Errorf("ResumeArgs = %q", got)
	}
}
