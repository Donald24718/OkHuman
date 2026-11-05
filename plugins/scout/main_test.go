package main

import (
	"os"
	"path/filepath"
	"testing"
)

func mkDoc(t *testing.T, dir, name, content string) {
	t.Helper()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "meta.json"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// keywords 写成数组 / 写成字符串 / 写成垃圾：都不能把整条文档弄丢。
func TestKeywordsTolerant(t *testing.T) {
	root := t.TempDir()
	mkDoc(t, filepath.Join(root, "arr"), "arr", `{"summary":"多卡训练排障","keywords":["多卡","显存","CUDA"]}`)
	mkDoc(t, filepath.Join(root, "str"), "str", `{"summary":"看图","keywords":"读图 图片"}`)
	mkDoc(t, filepath.Join(root, "bad"), "bad", `{"summary":"坏字段","keywords":{"a":1}}`)
	docs, probs := scanCorpus(Config{PluginRoot: root})
	if len(docs) != 3 {
		t.Fatalf("三条都该进索引，实际 %d", len(docs))
	}
	var gotA string
	for _, d := range docs {
		if d.Name == "arr" {
			gotA = d.Keywords
		}
	}
	if gotA == "" {
		t.Fatalf("数组形式的 keywords 没被读出来")
	}
	if len(probs) == 0 {
		t.Fatalf("坏字段应记 problem，实际 0 条")
	}
}

// meta.json 解析失败必须可见，不能静默少一条。
func TestBrokenMetaVisible(t *testing.T) {
	root := t.TempDir()
	mkDoc(t, filepath.Join(root, "ok"), "ok", `{"summary":"能用"}`)
	mkDoc(t, filepath.Join(root, "broken"), "broken", `{"summary":"没闭合的 json"`)
	docs, probs := scanCorpus(Config{PluginRoot: root})
	if len(docs) != 1 {
		t.Fatalf("只该有 1 条，实际 %d", len(docs))
	}
	if len(probs) != 1 {
		t.Fatalf("解析失败应报 1 条 problem，实际 %d", len(probs))
	}
}

// 同名不自动取舍：两条都要在（agent 自己决定删哪个）。
func TestSameNameKept(t *testing.T) {
	root := t.TempDir()
	mkDoc(t, filepath.Join(root, "a", "dup"), "dup", `{"summary":"第一份"}`)
	mkDoc(t, filepath.Join(root, "b", "dup"), "dup", `{"summary":"第二份"}`)
	docs, _ := scanCorpus(Config{PluginRoot: root})
	if len(docs) != 2 {
		t.Fatalf("同名两条都该保留，实际 %d", len(docs))
	}
}

// 内联「关键词：…」折进 keywords，且从 summary 摘掉（同一信号不算两遍）。
func TestSplitInlineKeywords(t *testing.T) {
	s, k := splitInlineKeywords("看图读图看视频。关键词：读图 识图 OCR", "")
	if s != "看图读图看视频。" {
		t.Fatalf("summary 没摘干净: %q", s)
	}
	if k != "读图 识图 OCR" {
		t.Fatalf("内联关键词没折进来: %q", k)
	}
	s2, k2 := splitInlineKeywords("正文 关键词：内联", "显式词")
	if s2 != "正文" || k2 != "显式词 内联" {
		t.Fatalf("显式+内联合并不对: %q %q", s2, k2)
	}
}

// 打分：只有 keywords 命中时必须恰好拿到 0.35，且不影响其他字段项。
func TestScoreWeights(t *testing.T) {
	d := Doc{Name: "demo", Keywords: "多卡 显存", Summary: "无关内容", Path: "/x/demo", Kind: "plugin"}
	b := scoreDoc(d, tokenize("多卡"), "多卡")
	if b.KwKw != 1 {
		t.Fatalf("kwKw 应为 1，得 %v", b.KwKw)
	}
	if b.Score != 0.35 {
		t.Fatalf("只命中 keywords 时得分应恰为 0.35，得 %v", b.Score)
	}
	if b.SumKw != 0 {
		t.Fatalf("不该命中 summary，得 %v", b.SumKw)
	}
}

// kind 必须分对：skills 树下的算 skill，其余算 plugin。
func TestKindOf(t *testing.T) {
	sep := string(filepath.Separator)
	if got := kindOf(sep + "P" + sep + "OkHuman_skill" + sep + "year" + sep + "x"); got != "skill" {
		t.Fatalf("skills 树下应为 skill，得 %q", got)
	}
	if got := kindOf(sep + "P" + sep + "Frequently Used Plugins" + sep + "x"); got != "plugin" {
		t.Fatalf("插件目录应为 plugin，得 %q", got)
	}
}

// 分词是覆盖率的地板：中文取单字+相邻二字，英文数字取整词。
func TestTokenizeCJK(t *testing.T) {
	toks := tokenize("看图 k3")
	want := map[string]bool{"看": true, "图": true, "看图": true, "k3": true} // 英文+数字连续串是一个整词
	for w := range want {
		found := false
		for _, tk := range toks {
			if tk == w {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("缺少 token %q（实际 %v）", w, toks)
		}
	}
}

// 名字在剪枝名单里、但目录自己带 meta.json → 不能被剪（skill 嵌在插件目录里也要能搜到）。
func TestAssetDirWithMetaNotSkipped(t *testing.T) {
	root := t.TempDir()
	mkDoc(t, filepath.Join(root, "host", "MiniMax-H3-Skills", "inner"), "inner", `{"summary":"内层能力","keywords":"内层"}`)
	docs, _ := scanCorpus(Config{PluginRoot: root})
	if len(docs) != 1 {
		t.Fatalf("带 meta.json 的目录不该被剪掉，实际 %d 条", len(docs))
	}
}

// 子技能不独立入索引：外层目录有 meta.json，内层再有一个也只算外层的一条。
func TestNestedSubSkillNotIndexed(t *testing.T) {
	root := t.TempDir()
	mkDoc(t, filepath.Join(root, "parent"), "parent", `{"summary":"母文档：路由到子技能","keywords":"母"}`)
	mkDoc(t, filepath.Join(root, "parent", "sub-skill"), "sub-skill", `{"summary":"子技能自己也有 meta.json","keywords":"子"}`)
	docs, _ := scanCorpus(Config{PluginRoot: root})
	if len(docs) != 1 {
		t.Fatalf("只该索引母文档一条，实际 %d: %v", len(docs), docs)
	}
	if docs[0].Name != "parent" {
		t.Fatalf("该索引的是母文档，得 %q", docs[0].Name)
	}
}

func TestKindOfPublicLayout(t *testing.T) {
	sep := string(filepath.Separator)
	if got := kindOf(sep + "repo" + sep + "plugins" + sep + "skills" + sep + "x"); got != "skill" {
		t.Fatalf("公开仓 skills/ 布局应判为 skill，得 %q", got)
	}
}
