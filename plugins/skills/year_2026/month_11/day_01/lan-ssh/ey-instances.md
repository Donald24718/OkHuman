## 7. 另一台 上的 OkHuman 实例（**2026-11-05 更正**；原小节写于 2026-11-02）

| 实例 | 端口 | 后端 | 模型/上下文 | 卡 | 数据目录 |
|---|---|---|---|---|---|
| **小E**（原小D） | `:8451` | `:8080` **strata-volta-0.2.10**（2026-11-05 起；**旧 0.1.44.1 树当天已删除**，回滚后端要重新装；`llama-pin/` 已移进 0.2.10 树保留，将来能重编） | 122B iq3s / 512k 窗口 | GPU1（实测 14.1G，含常驻视觉） | `$OKHUMAN_DATA_DIR`（2026-11-12 实测改名） |
| **小C**（原小F） | `:8452` | `:8081` kvmem-ninfer-v100 | qwen3.8-27b / 200k | GPU0（实测 15.4G） | `$OKHUMAN_DATA_DIR`（2026-11-12 实测改名） |

`.3` 上的仓库/实例目录清单见 `host-crash-autostart/services.md`「另一台机器」一节。对外入口只有一条 `192.168.1.3:8602`，多实例必须点名 `okh_inst`（8603 已 2026-11-12 删）。sd-box 上那台「四实例入口页」是 root 属主的 `/opt/okhuman-entries/entries.py`（unit `okhuman-entries.service`）：UP 元组末位是 force_inst，指 `.3` 的两条必须填 8451/8452 且用 8602 的 token；改完 `sudo systemctl restart okhuman-entries.service`；验证法=带 `okh_bridge_<tag>` cookie 打 `127.0.0.1:184xx/status`，再拿 `raw_tail` 指纹与直连点名上游比对（2026-11-12 这样证到过 18502→8452）。

### 7.1 起法：systemd 管的；**2026-11-08 起全部迁到 ey 的 user 单元**（旧「system 单元、root 跑」已作废）

`~/.config/systemd/user/` 下 9 个单元全部 `enabled`：`okhuman-8451`、`okhuman-8452`、`strata-8080`、`ninfer-8081`、`lan-bridge-8451`、`okhuman-cron`、`okhuman-scout`、`okmon`、`qq-bridge`（2026-11-16 实测）。`/etc/systemd/system/` 下同名的已在 11-08 改名 `*.disabled-20261108`（含 `lan-bridge-8452`）。进程属主 = `ey`，不是 root。

- 管理（2026-11-16 实测）：**不用 sudo**。非登录 ssh 里没有 bus，先 `export XDG_RUNTIME_DIR=/run/user/$(id -u); export DBUS_SESSION_BUS_ADDRESS=unix:path=$XDG_RUNTIME_DIR/bus`，再 `systemctl --user restart okhuman-8451 okhuman-8452`。写成 `sudo systemctl restart` 会得到 `Unit okhuman-8451.service not found`（我 11-16 踩的，别据此断定没有这个单元）。
- **别照 `/home/ey/bin/start-okhuman-845*.sh` 判断跑的是什么**：单元现在直接 `ExecStart=~/hdd/OkHuman{,-8452}/okhuman`（2026-11-12 用 `readlink /proc/<MainPID>/exe` 实测对上），那两个包装脚本已不被引用（8451 那份还写 `exec ./okhuman-local`，而 `okhuman-local` 早不存在了）。判「跑的是哪个文件」只认 `/proc/<pid>/exe`。
- 原文那套手工 `setsid nohup …` 现在照抄会端口撞车，只当历史读（见 §7.4）。

### 7.2 `.3` 上没有 git（2026-11-05 已装 git 2.43.0）

那两棵树**是 git checkout**（有自己的提交历史，他们自己提交过 prompt 铁律），但 11-05 之前 `.3` 没装 git → 只能用文件同步。

- rsync 必排除：`.git`、`/config/`、`/prompts/01-identity.md`、`README.md`、`README-overview.md`、`avatar/`、`okhuman.bak-*`、`OKHUMAN_UNIFORM.md`、`*.log`。
- **别加 `--delete` / `--delete-excluded`** → 会把对面的 `config/`、`.git/`、`avatar/` 删掉（2026-11-05 dry-run 里发现，差点执行）。
- 2026-11-05 已把工作区规范化：各自的自有改动一条 commit、生产同步来的代码一条 commit；`.gitignore` 补了 `/okhuman-local`、`okhuman.bak-*`、`/okhuman-local.bak-*`（之前 `/okhuman` 有、`okhuman-local` 漏，所以它一直挂未跟踪）。
- 从 `.3` **能** fetch 到 GitHub 公开仓 `Donald24718/OkHuman`（实测退出码 0，代码路径差异为空 = 同步内容一致）；但**两边历史无共同祖先**，不能直接 pull/merge。git 在这两边只适合 `status`/`diff` 审计。

### 7.2b 换推理后端（0.1.44.1 → 0.2.10，2026-11-05 实测做完一遍）

- 重编依赖：`build-ab` 当初是用旧树的 `llama-pin/` 编的，旧树删了之后它已随树保留在 `strata-volta-0.2.10/llama-pin`。增量重编要把 CMake 的 `FETCHCONTENT_SOURCE_DIR_STRATA_LLAMACPP` 指到这个新位置（旧缓存里还是旧树的绝对路径）。
- 自足做法：整棵树 rsync 到 `.3`（排除 `.venv`、`logs/`、`__pycache__/`、`*.bak-*`；局域网 ~100 MiB/s，14 G 约 2.5 分钟），**`.venv` 在目标机现建现装**（`python3.12 -m venv .venv && .venv/bin/python -m pip install -r requirements.txt`）。两边 glibc 2.39 / CUDA 12.8 / Python 3.12.3 完全一致 → 生产侧编好的 `build-ab/strata` 在 `.3` 直接能跑（先 `./build-ab/strata --help` 冒烟再切）。
- **坑（我踩过，白等 2 分钟）：由旧配置派生新配置时，只替换路径前缀不够——旧树的构建目录叫 `build/`，我这棵树叫 `build-ab/`。** `exe` 会指到不存在的文件，单元 `Restart` 空转、GPU 只剩几 MiB。改完必须核对 `exe`、`vision.exe`、`tokenizer`、`--pack/--native/--ple-gguf/--mtp/--expert-profile/--control-vector-scaled` 每一项是否真存在。
- 要改的两处都有备份：包装 `/home/ey/bin/start-strata-8080.sh`（**root 所有，要 sudo**；旧写法 `…sh.bak-2026-11-05-pre-0210`）和单元里写死旧树路径的 `ExecStartPre`（`/etc/systemd/system/strata-8080.service.bak-2026-11-05-pre-0210`）。切换用 `sudo -S -k bash <脚本>`：`reset-failed` + `restart`，装载 1.5-3 分钟。
- 参数保持的做法：去审核（`--control-vector-scaled … --cvec-mode project --cvec-dir per-layer`）会随配置一起搬过去，启动日志里能看到 `control vector mode = project … (41 steered)` 才算真生效。
- **视觉**：0.2.10 在 <23.5 GB 的卡上默认把图像编码器放 CPU（`--vram-reserve-mib 700` 时更是"起不来→CPU"）。要常驻 GPU 就 `"elastic": false` + `--vram-reserve-mib 2048`，日志出现 `warmed up at 4096 image tokens` 即成；**代价约 955 个专家槽（3275 → 2320）**，不看图就别开。

### 7.3 健康检查（完整版，判定只在控制侧做）

```
ssh ey 'for p in 8080 8081 8451 8452 8480 8496 8602 8603; do printf "%s " $p; curl -s -m 5 -o /dev/null -w "%{http_code}\n" http://127.0.0.1:$p/; done'
```
8080/8081 是后端，8451/8452 是实例，**8480 scout / 8496 okmon / 8602 一条 lan-bridge（8603 已于 2026-11-12 删，探到 000/连接拒绝才对）**（`.3` 的 aux 是单元；本机的 aux 不是单元，见 `host-crash-autostart`）。
判实例跑的是新代码还是旧代码：**别拿 `/health` 的 `summary_limit` 当判据**——旧版也有该字段（=5），新版若该实例 config 写了 `keep_summaries: 5` 也照旧显示 5（它取的是 config 原值，实际窗口已被 `context.NormalizeCfg` 强制成 1）。可靠三条：`/proc/<pid>/exe` 有无 `(deleted)` 及其 mtime 是否等于新编译时间；`go version -m` 的 `vcs.revision`；拿本次改动新增/被删的字符串 `grep -acF` 对照（2026-11-16 用第三条证到过三棵山寨树少一段 race 修复）。

### 7.4 历史（2026-11-02 的手动起法与实测数，已作废）

**实测**：8080 生成 **37–46 tok/s**（同模型在我这台 31.2，因为它独占卡）；strata 冷启动 **约 5 分钟**（79G 权重进内存映射）；kvmem 8081 加载约 1 分钟。
