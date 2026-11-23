package main

// 提示词目录的三条优先级（2026-11-12 从 main() 抽出后补测）。

import (
	"path/filepath"
	"testing"
)

func TestPromptDirFromArgs(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want string
		err  bool
	}{
		{"无参数", nil, "", false},
		{"只有 flag（本程序不接 flag，一律当用法错）", []string{"-config", "x"}, "", true},
		{"空串", []string{""}, "", true},
		{"给目录", []string{"/opt/prompts"}, "/opt/prompts", false},
		{"相对路径按当前目录解析", []string{"prompts"}, filepath.Join("/proc-cwd-cannot-check", "x"), false}, // 只验不报错
		{"多取第一个", []string{"/a", "/b"}, "/a", false},
	}
	for _, c := range cases {
		got, err := promptDirFromArgs(c.args)
		if c.err {
			if err == nil {
				t.Errorf("%s：应报用法错，实得 %q", c.name, got)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s：不该报错：%v", c.name, err)
			continue
		}
		if c.name == "相对路径按当前目录解析" {
			if !filepath.IsAbs(got) {
				t.Errorf("相对路径应解析成绝对路径，实得 %q", got)
			}
			continue
		}
		if got != c.want {
			t.Errorf("%s：got %q want %q", c.name, got, c.want)
		}
	}
}

func TestResolvePromptDirPriority(t *testing.T) {
	if got := resolvePromptDir("/cmd/dir", "/root", "prompts"); got != "/cmd/dir" {
		t.Errorf("命令行目录必须优先，实得 %q", got)
	}
	if got := resolvePromptDir("", "/root", "/abs/prompts"); got != "/abs/prompts" {
		t.Errorf("config 给绝对目录应直接用它，实得 %q", got)
	}
	if got := resolvePromptDir("", "/root", "prompts"); got != filepath.Join("/root", "prompts") {
		t.Errorf("config 相对目录应拼到项目根，实得 %q", got)
	}
	if got := resolvePromptDir("", "/root", ""); got != "/root" {
		t.Errorf("config 目录为空时拼根（prompts.LoadPromptDir 会报缺目录），实得 %q", got)
	}
}
