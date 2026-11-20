# computer — 往应用里输入文本

## 做法

- **长文本走剪贴板**：`(setsid xclip -selection clipboard -i 文件 &)` 然后 `key ctrl+v`。**每次粘贴前重新装填**（xclip 服务过一次可能已退出，前台 xclip 一退内容就没了）。
- 起 xclip（以及一切不退出才干活的进程）必须脱离三件套：`printf … | setsid xclip -selection clipboard -d :0 >/dev/null 2>&1 &`——**管道喂输入时不要加 `< /dev/null`**（它会把管道顶掉、粘贴内容为空）。缺重定向时 xclip 持有 selection 不退出、还占着输出管道写端 → 命令被挂住直到下一个 xclip 顶替它，实测挂等 167–225 秒（对照实验见 `LESSONS.md` 2026-10-31）。
- 纯中文可走 `$XU key U+码点`（逐码点，免剪贴板）。

## 已知不靠谱的输入方式（实测）

- `$XU type` 会**吞连字符**（`creator-micro` → `creatormicro`）；混排 ASCII 会被中文输入法当拼音吃、标点丢 → **带符号的串一律走粘贴**。
- `xdotool key` 拒 `.` 和 `-`（当 key-sequence 分隔符）：含 `llama.cpp`、`-np` 的文本会丢字符 → 用 keysym 名 `period` `minus` `slash` `parenleft/parenright` `braceleft/braceright` `comma`。
- **type 的焦点路由非原子**：同一字符串的字符可散落到不同窗口或丢失（实测 16 字符 → 1 进 xed、7 进 terminal、8 丢失），还会顺序错位（开头三个字跑到末尾）。**重要输入后必须截图逐字核对再提交。**
- **微信 Linux 4.x 的输入框里 `type` 完全失效**（中文 ASCII 都进不去，stderr 报 `Serial number of failed request`），但直接喂 `key a` / `key U4F60` 正常 → 自己逐 keysym 喂（配方见 `LESSONS.md` 2026-10-01）。
- 单实例应用不是 type 的干净靶（`open xed` 不带参数会把文档挂进用户已有窗口的标签）→ 测输入要带文件参数或选多实例应用。
- 应用快捷键可能被用户重映射（本机 xed 切标签是 Alt+1..9）→ 键盘操作前先查 `~/.config/<app>/accels`。
- `tesseract`（装有 chi_sim）对微信这类小字输出乱码，**OCR 标定这条路在本机不可用**。
