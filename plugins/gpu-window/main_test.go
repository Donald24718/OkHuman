package main

import (
	"os"
	"strconv"
	"testing"
)

func TestStrListBothForms(t *testing.T) {
	var a strList
	if err := a.UnmarshalJSON([]byte(`"x"`)); err != nil || len(a) != 1 || a[0] != "x" {
		t.Fatal("字符串形式没接住")
	}
	var b strList
	if err := b.UnmarshalJSON([]byte(`["y","z"]`)); err != nil || len(b) != 2 {
		t.Fatal("数组形式没接住")
	}
}

func TestParseMaps(t *testing.T) {
	g := parseGPUs("0, GPU-aaa, Tesla V100, 14142 MiB\n1, GPU-bbb, Tesla V100, 8 MiB")
	if len(g) != 2 || g[0].Index != "0" || g[0].UUID != "GPU-aaa" {
		t.Fatalf("GPU 行解析错: %+v", g)
	}
	self := strconv.Itoa(os.Getpid())
	a := parseApps("GPU-aaa, " + self + ", 14118 MiB")
	if len(a) != 1 || a[0].MiB != 14118 || a[0].PID != self {
		t.Fatalf("compute-apps 解析错: %+v", a)
	}
}

func TestResolvePidsOnlyTheHolder(t *testing.T) {
	me := strconv.Itoa(os.Getpid())
	apps := []appRow{{PID: me, Cmdline: "/x/build-ab/strata --serve --pack p"}, {PID: "999", Cmdline: "python -m serve.server"}}
	svc := service{Name: "8080", Match: strList{"build-ab/strata", "build-vision-cuda/bin/strata-vision"}}
	got := resolvePids(svc, apps)
	if len(got) != 1 || got[0] != me {
		t.Fatalf("应只挑出持有显存的进程，得到 %v（MainPID 陷阱就是挑错了 python 主进程）", got)
	}
}

func TestGateBlocksOnRunning(t *testing.T) {
	if !bodyBlocks([]byte(`{"running":true}`), "running") {
		t.Fatal("running=true 应该拦住")
	}
	if bodyBlocks([]byte(`{"running":false}`), "running") {
		t.Fatal("running=false 不该拦")
	}
	if bodyBlocks([]byte(`{`), "running") || bodyBlocks([]byte(`{"other":true}`), "running") {
		t.Fatal("坏响应/缺字段不该拦")
	}
}

func TestStateNamesMatchDriverEnum(t *testing.T) {
	// 0 RUNNING / 1 LOCKED / 2 CHECKPOINTED（成功让卡）/ 3 FAILED —— 曾被误读成"异常"
	for i, want := range []string{"RUNNING", "LOCKED", "CHECKPOINTED（已让卡）", "FAILED"} {
		if got := stateName(i); got != want {
			t.Fatalf("state %d 应为 %s，得到 %s", i, want, got)
		}
	}
}
