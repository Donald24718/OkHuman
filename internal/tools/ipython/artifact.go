package ipython

// 富展示产物落盘：图片 / PDF 这类 MIME 既内联不进对话（base64 会吃掉整轮配额），
// 又不能静默丢弃（否则模型以为自己的绘图代码没生效）。
//
// 落盘 + 回传绝对路径是唯一两头都成立的方案：模型拿到路径后可以用 bash 读，
// 或交给 attach 一类的素材插件真正"看见"这张图。

import (
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"
)

// artifactSpec 落盘规则：扩展名 + 载荷是否为 base64。
type artifactSpec struct {
	ext    string
	binary bool
}

// artifactMIME 可落盘的 MIME。
//
// ⚠️ SVG 是**纯文本**不是 base64（Jupyter 的约定），按 base64 解码会直接失败——
// 这条是实测踩出来的，别和 image/png 归为一类。
var artifactMIME = map[string]artifactSpec{
	"image/png":       {".png", true},
	"image/jpeg":      {".jpg", true},
	"image/gif":       {".gif", true},
	"image/webp":      {".webp", true},
	"application/pdf": {".pdf", true},
	"image/svg+xml":   {".svg", false},
}

// Artifact 一件已落盘的产物。
type Artifact struct {
	MIME  string // 原始 MIME
	Path  string // 绝对路径（给模型用）
	Bytes int    // 落盘字节数
}

var (
	artifactDir atomic.Pointer[string]
	artifactSeq atomic.Int64
)

// SetArtifactDir 设置落盘根目录（传空串恢复默认）。
//
// 由 main 用数据目录接线（`<dataDir>/ipython-output`）。不接线也能用：
// 退到临时目录下的专属子目录，功能不降级，只是文件不随实例持久化。
func SetArtifactDir(dir string) {
	d := strings.TrimSpace(dir)
	artifactDir.Store(&d)
}

// artifactRoot 落盘目录。
func artifactRoot() string {
	if p := artifactDir.Load(); p != nil && strings.TrimSpace(*p) != "" {
		return filepath.Join(filepath.Clean(*p), "ipython-output")
	}
	return filepath.Join(launcherDir(), "output")
}

// saveArtifacts 把 res 里无法内联的 MIME 落盘，并从原 map 中**摘掉**。
//
// 摘掉是必需的：不然 resultNote 还会照旧提示"无法内联、请 savefig 落盘"，
// 与"已经落好了、路径在下面"自相矛盾，模型会被这段过期建议带偏。
func saveArtifacts(res *ExecResult) {
	var save func(m map[string]string)
	save = func(m map[string]string) {
		if len(m) == 0 {
			return
		}
		for mime, data := range m {
			spec, ok := artifactMIME[mime]
			if !ok || strings.TrimSpace(data) == "" {
				continue
			}
			path, n, err := writeArtifact(mime, spec, data)
			if err != nil {
				continue // 落盘失败就让它留在原处，至少还有"无法内联"的提示兜底
			}
			res.Saved = append(res.Saved, Artifact{MIME: mime, Path: path, Bytes: n})
			delete(m, mime)
		}
	}
	save(res.Result)
	for _, d := range res.Displays {
		save(d)
	}
	// 落盘顺序按 MIME 稳定排序，避免 map 遍历顺序让输出文本抖动（KV 前缀敏感）。
	sortArtifacts(res.Saved)
}

// writeArtifact 解码并写盘，返回路径与字节数。
func writeArtifact(mime string, spec artifactSpec, data string) (string, int, error) {
	data = strings.TrimSpace(data)
	raw := []byte(data)
	if spec.binary {
		b, err := base64.StdEncoding.DecodeString(data)
		if err != nil {
			return "", 0, fmt.Errorf("%s 不是合法 base64: %w", mime, err)
		}
		raw = b
	}
	dir := artifactRoot()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", 0, err
	}
	n := artifactSeq.Add(1)
	// 文件名带时间戳与序号：时间戳便于人找，序号保证同一秒内的多次调用不撞名。
	name := fmt.Sprintf("out-%s-%d%s", time.Now().Format("20060102-150405"), n, spec.ext)
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		return "", 0, err
	}
	return path, len(raw), nil
}

// sortArtifacts 按 MIME 再按路径排序（输出文本确定性）。
func sortArtifacts(a []Artifact) {
	for i := 1; i < len(a); i++ {
		for j := i; j > 0; j-- {
			if a[j].MIME < a[j-1].MIME || (a[j].MIME == a[j-1].MIME && a[j].Path < a[j-1].Path) {
				a[j], a[j-1] = a[j-1], a[j]
				continue
			}
			break
		}
	}
}

// humanBytes 给模型看的大小（保留一位小数，够判断"图是不是空的"）。
//
// ⚠️ 单位与除法次数必须对齐：先除一次才是 KB。写错一档的后果很隐蔽——
// 端到端实测把 19139 字节（18.7 KB）报成了 "18.7 MB"，模型会据此误判
// 这张图大到不该读。由 TestHumanBytes 守着。
func humanBytes(n int) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	v := float64(n) / unit // 除一次 → KB
	units := []string{"KB", "MB", "GB"}
	i := 0
	for v >= unit && i < len(units)-1 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f %s", v, units[i])
}
