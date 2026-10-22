package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

// 前端净化（webui/sanitize.js）回归：bot 回复走 marked → innerHTML，不净化就是 XSS。
// 需要 node（+ jsdom，用 OKHUMAN_JSDOM 指定模块路径）；缺则 skip，不挡无 node 环境。
func TestWebuiSanitize(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("无 node，跳过前端净化测试")
	}
	jsdom := os.Getenv("OKHUMAN_JSDOM")
	if jsdom == "" {
		jsdom = "jsdom"
	}
	// 解析不到 jsdom 就 skip，不是 fail。测试的价值是"能跑的环境里真跑"，
	// 不该因为环境缺依赖把整条流水线判死——GitHub runner 自带 node 但不带
	// jsdom，v0.6.0 的 Release 就是这么三个平台全挂的（发布物没发出去）。
	if err := exec.Command(node, "-e", "require.resolve(process.argv[1])", jsdom).Run(); err != nil {
		t.Skipf("无 jsdom（%s），跳过前端净化测试；装好后用 OKHUMAN_JSDOM 指模块路径即可真跑", jsdom)
	}
	cmd := exec.Command(node, filepath.Join("webui", "sanitize_test.mjs"))
	cmd.Env = append(os.Environ(), "OKHUMAN_JSDOM="+jsdom)
	out, err := cmd.CombinedOutput()
	t.Logf("%s", out)
	if err != nil {
		t.Fatalf("前端净化测试未通过：%v", err)
	}
}
