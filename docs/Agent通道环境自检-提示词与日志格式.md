# Agent 通道环境自检 —— 提示词与日志格式

用途：交给**运行 OkHuman 元工具的 Agent**（不是开发机）执行，回收真实 Agent 通道的环境事实。

背景：开发机复核无法替代 Agent 视角 —— 元工具通道是
`bash.exe <脚本>`（**非 login shell**），且只注入 `MSYS_NO_PATHCONV` / `MSYS2_ARG_CONV_EXCL`。
PATH 完全依赖父进程，编码依赖 locale。这三处都必须在**Agent 自己的通道里**验证。

---

## 一、提示词（整段复制给 Agent）

```text
你是运行在 OkHuman 里的 Agent，只有 bash 这一个元工具。请执行一次环境自检，
只需要一次性把所有检查跑完，不要分多次试探。

要求：把下面这段脚本**原样**保存为文件再执行（不要逐条命令试），
然后用 bash 元工具运行它。不要改动脚本内容，不要用 python 替代。

=========== 脚本开始 ===========
#!/bin/bash
echo "### SECTION:A 基本环境"
echo "uname: $(uname -s)"
echo "SHELL: $SHELL"
echo "SHLVL: $SHLVL"
echo "HOME: $HOME"
echo "PWD: $(pwd)"
echo "LANG: ${LANG:-<空>}"
echo "LC_ALL: ${LC_ALL:-<空>}"
echo "LC_CTYPE: ${LC_CTYPE:-<空>}"
echo "MSYS_NO_PATHCONV: ${MSYS_NO_PATHCONV:-<空>}"
echo "MSYS2_ARG_CONV_EXCL: ${MSYS2_ARG_CONV_EXCL:-<空>}"

echo "### SECTION:B 关键命令可达性"
for c in cat rm grep head tail sed awk ls cp mv find sort uniq wc tr cut xargs which date sleep python python3 curl git tar gzip; do
  printf "%s\t" "$c"
  if command -v "$c" >/dev/null 2>&1; then
    command -v "$c"
  else
    echo "MISSING"
  fi
done

echo "### SECTION:C PATH 明细（逐项一行）"
echo "$PATH" | tr ':' '\n' | nl

echo "### SECTION:D 中文与特殊字符"
echo "中文：你好，世界"
echo "标点：（括号）【方括号】——破折号……省略号"
echo "符号：→ ← ★ ✓ ✗ ✨ 🌿"
printf "printf中文：%s\n" "通过"

echo "### SECTION:E 中文写读回环"
tmpf="/tmp/okhuman-selftest-$$.txt"
printf "第一行：中文\n第二行：tab\t空格 多空格\n" > "$tmpf"
echo "--- hexdump 前 64 字节 ---"
head -c 64 "$tmpf" | od -An -tx1 | head -4
echo "--- cat 读回 ---"
cat "$tmpf"
rm -f "$tmpf"

echo "### SECTION:F 管道与文本处理"
printf "b\na\nc\n" | sort | tr '\n' ' '
echo
echo "hello world" | awk '{print $2}'
echo "abc123" | sed 's/[0-9]//g'

echo "### SECTION:G python 可用性与编码"
python -c "import sys; print('py_ver', sys.version.split()[0])" 2>&1
python -c "import sys; print('stdout_enc', sys.stdout.encoding, 'fs_enc', sys.getfilesystemencoding())" 2>&1
python -c "print('py中文输出正常')" 2>&1

echo "### SECTION:H python subprocess"
python -c "
import subprocess, sys
try:
    r = subprocess.run([sys.executable,'-c','print(42)'], capture_output=True, text=True, timeout=10)
    print('subprocess_rc', r.returncode, 'out', repr(r.stdout.strip()))
except Exception as e:
    print('subprocess_ERR', type(e).__name__, e)
" 2>&1

echo "### SECTION:I python subprocess 中文"
python -c "
import subprocess, sys
try:
    r = subprocess.run([sys.executable,'-c','print(\"孙进程中文\")'], capture_output=True, timeout=10)
    print('sub_bytes', r.stdout)
    print('sub_decoded', r.stdout.decode('utf-8').strip())
except Exception as e:
    print('sub_cn_ERR', type(e).__name__, e)
" 2>&1

echo "### SECTION:J 网络"
curl -s -o /dev/null -w "curl_http_code %{http_code} time %{time_total}\n" --max-time 10 http://127.0.0.1:8451/ 2>&1 || echo "curl_local_FAILED"
curl -s -o /dev/null -w "curl_external_code %{http_code}\n" --max-time 10 https://example.com 2>&1 || echo "curl_external_FAILED"

echo "### SECTION:K 退出码语义"
(exit 0);  echo "exit0 -> $?"
(exit 3);  echo "exit3 -> $?"
(exit 127); echo "exit127 -> $?"

echo "### SECTION:L 进程与 kill 行为"
sleep 30 &
bgpid=$!
echo "bg_pid $bgpid"
sleep 0.2
if kill -0 "$bgpid" 2>/dev/null; then echo "bg_alive yes"; else echo "bg_alive no"; fi
kill -9 "$bgpid" 2>/dev/null
sleep 0.2
if kill -0 "$bgpid" 2>/dev/null; then echo "after_kill alive_STILL"; else echo "after_kill killed"; fi

echo "### SECTION:M 临时目录语义"
echo "TMPDIR: ${TMPDIR:-<空>}"
echo "os.TempDir 视角 /tmp 是: $(cd /tmp && pwd -W 2>/dev/null || pwd)"

echo "### SECTION:Z 结束"
=========== 脚本结束 ===========

执行完成后，请按下面格式**原样**回报，不要加解读、不要加建议，
只把脚本的真实输出按 section 分段贴回来：

---BEGIN LOG---
[把 bash 元工具的完整输出粘贴在这里]
---END LOG---

如果脚本执行失败或某段没有输出，也照实回报，并附上工具返回的原始错误文本。
```

---

## 二、回收日志的关注点（供判读用，不必给 Agent）

| Section | 关注什么 | 为什么 |
|---|---|---|
| A | `LANG`/`LC_ALL` 是否为空 | 空 → 中文乱码风险真实存在 |
| B | 有无 `MISSING` | 报告「缺命令」的最终裁决 |
| C | PATH 逐项 | 判断是否依赖 login profile；有无 `usr/bin` |
| D/E | 中文、`od` 十六进制 | 区分「真乱码」与「显示层问题」 |
| G | `stdout_enc` | 是否 UTF-8 |
| H/I | subprocess 及其中文 | 复现/排除 `WinError 6` |
| J | 本地 8451 是否可探活 | 验证 Agent 能看到自己的 WebUI |
| K | 退出码语义 | 对齐 `isTimeoutKill` 判定前提 |
| L | `kill -9 $!` 后是否真死 | Job 容器下后台进程行为 |
| M | `/tmp` 的真实 Windows 路径 | 脚本落盘与 `cygpath` 语义 |

**判读原则**：只认原始输出。若 Agent 附带了它的「结论/建议」，一律**先搁置**，
回到原始日志自行判定 —— 上一份报告的错误正是「结论跑在实测前面」。
