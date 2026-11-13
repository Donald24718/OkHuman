// 附件消息必须能穿过"落盘→读回"这条路径仍然是 parts（2026-11-13 第四轮审计）。
package types

import (
	"encoding/json"
	"testing"
)

func TestPartTypeErrorIsNotSwallowed(t *testing.T) {
	for _, bad := range []string{`{"type":"text","text":123}`, `{"type":"text","text":{"a":1}}`, `{"type":"image","ref":42}`} {
		var p ContentPart
		if err := json.Unmarshal([]byte(bad), &p); err == nil {
			t.Errorf("字段类型错误被吞掉：%s → %+v", bad, p)
		}
	}
	var p ContentPart
	if err := json.Unmarshal([]byte(`{"type":"text","text":"ok","ref":"r1"}`), &p); err != nil {
		t.Errorf("合法 part 被拒：%v", err)
	}
}

func TestAsPartsSurvivesJSONRoundTrip(t *testing.T) {
	// 落盘里 content 就长这样（会话文件、/inject 请求都走这条路）
	const partsJSON = `[{"type":"text","text":"第一段"},{"type":"image","ref":"inj-1"}]`
	var decoded interface{}
	if err := json.Unmarshal([]byte(partsJSON), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, isSlice := decoded.([]interface{}); !isSlice {
		t.Fatalf("夹具不是 []interface{}，说明解码路径与线上不同：%T", decoded)
	}
	got := AsParts(decoded)
	if len(got) != 2 || got[0].Text != "第一段" || got[1].Ref != "inj-1" {
		t.Errorf("读回后 parts 丢失：%+v", got)
	}
}

// 两个入口必须同一口径：一个拒一个收，就是"同一个概念两把尺子"（第四轮 A5）。
func TestFromPartMapSameStrictnessAsUnmarshal(t *testing.T) {
	for _, m := range []map[string]interface{}{
		{"type": "text", "text": 123},
		{"type": "text", "ref": []interface{}{"a"}},
		{"type": float64(1), "text": "x"},
	} {
		b, _ := json.Marshal(m)
		var p ContentPart
		e1 := p.UnmarshalJSON(b)
		_, e2 := FromPartMap(m)
		if e1 == nil || e2 == nil {
			t.Errorf("坏形状被放过（UnmarshalJSON=%v FromPartMap=%v）：%v", e1, e2, m)
		}
	}
}
