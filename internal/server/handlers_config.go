package server

import (
	"net/http"
	"okhuman/internal/textutil"
	"path/filepath"
	"strings"

	"okhuman/internal/persist"
)

func (ap *App) handleAttachments(w http.ResponseWriter, r *http.Request) {
	ap.state.mu.Lock()
	injects := append([]InjectInfo(nil), ap.state.Injects...)
	ap.state.mu.Unlock()
	writeJSON(w, 200, map[string]interface{}{"injects": injects})
}

func (ap *App) handlePrompts(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	a.promptMu.Lock()
	var dir interface{}
	if a.PromptDir != "" {
		dir = a.PromptDir
	}
	fallback := a.PromptFallback
	files := append([]PromptFileInfo(nil), a.PromptFiles...)
	sp := a.SystemPrompt
	a.promptMu.Unlock()
	writeJSON(w, 200, map[string]interface{}{
		"dir":      dir,
		"fallback": fallback,
		"files":    files,
		"chars":    textutil.UTF16Len(sp),
		"prompt":   sp,
	})
}

func (ap *App) handlePromptsFilePost(w http.ResponseWriter, r *http.Request) {
	b := tryReadJSON(r)
	a := ap.state.Agent
	if a.PromptDir == "" {
		writeJSON(w, 400, map[string]interface{}{"error": "无提示词目录（内置默认提示词，请用配置页 system_prompt.dir 建目录后重启）"})
		return
	}
	name, ok := b["name"].(string)
	if !ok || !safeName(name) {
		writeJSON(w, 400, map[string]interface{}{"error": "文件名非法（仅字母/数字/_/-/.，不能以 . 开头）"})
		return
	}
	content, ok := b["content"].(string)
	if !ok {
		writeJSON(w, 400, map[string]interface{}{"error": `需 { "name": "xx.md", "content": "..." }`})
		return
	}
	file := filepath.Join(a.PromptDir, name)
	if strings.HasSuffix(strings.ToLower(name), ".md") {
		// 新增 .md → 立即重载生效
		if err := persist.WritePath(file, content); err != nil {
			writeJSON(w, 500, map[string]interface{}{"error": "写入失败：" + err.Error()})
			return
		}
		reloadAgentPrompt(a)
		a.promptMu.Lock() // 2026-09-14 审计修：PromptFiles 读取与并发重载互斥
		names := make([]string, 0, len(a.PromptFiles))
		for _, f := range a.PromptFiles {
			names = append(names, f.Name)
		}
		a.promptMu.Unlock()
		writeJSON(w, 200, map[string]interface{}{"ok": true, "name": name, "reloaded": true, "files": names})
		return
	}
	if err := persist.WritePath(file, content); err != nil {
		writeJSON(w, 500, map[string]interface{}{"error": "写入失败：" + err.Error()})
		return
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "name": name, "reloaded": false})
}

func (ap *App) handlePromptsReload(w http.ResponseWriter, r *http.Request) {
	a := ap.state.Agent
	rl := reloadAgentPrompt(a)
	names := make([]string, 0, len(rl.Files))
	for _, f := range rl.Files {
		names = append(names, f.Name)
	}
	writeJSON(w, 200, map[string]interface{}{"ok": true, "fallback": rl.Fallback, "files": names, "chars": textutil.UTF16Len(rl.Prompt)})
}

// ---------- 配置（WebUI 配置页，2026-08-29，2026-09-09 单实例化） ----------
