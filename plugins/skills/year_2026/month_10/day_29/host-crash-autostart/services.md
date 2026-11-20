# 四个后端：位置、启动命令、探活（2026-10-29 实测）

本机：Linux Mint 22.3 / Xeon E5 / 251 GB DDR3 RECC（无 ECC 单条）。

| 端口 | 角色 | 位置 | 怎么起 | 自启 |
|---|---|---|---|---|
| 8451 | 生产实例（小A） | `~/okhuman` | `systemctl --user start okhuman-8451` | unit enabled |
| 8080 | **8451 的推理后端 = Strata 0.2.10** | `<本地盘>/strata-volta-0.2.10` | 平时重启用 `$D/restart-strata-8080.sh`（见「8080 的版本现况」）；冷启 `systemctl --user start strata-8080` | unit enabled |
| 8452 | 副本实例（小B） | `<副本树>/OkHuman-8452/okhuman` | `systemctl --user start okhuman-8452` | unit，enabled |
| 8081 | **8452 的推理后端 = ninfer** | `<本地盘>/kvmem-ninfer-v100` | `systemctl --user start ninfer-8081` | unit，enabled |
| 8480 | 插件/技能索引 scout | `plugins/scout/scout` | `systemctl --user start okhuman-scout` | unit，enabled |

- **两个实例的后端不是同一个服务、也不是同一个模型**：8451→8080 Strata（`packs/iq3s` + `mode_gguf/qwen3.8/122b`，262k 上下文，`gpu: 1`）；8452→8081 ninfer（`models/orca-iq3xxs.ninfer`，`--model-id qwen3.8-27b`，200k，`CUDA_VISIBLE_DEVICES=2`）。
- 谁连谁由各自的 `config/default.json → llm.base_url` 决定：生产 `http://127.0.0.1:8080/v1`，副本 `http://127.0.0.1:8081/v1`；模型名在 `config/user.json → llm.model` 覆盖（副本写的是 `qwen3.8-27b`，与 `--model-id` 必须一致）。改端口要同时改 `server.port` 和 `llm.base_url`。
- 数据目录：`config/user.json → data.dir`（生产 `$OKHUMAN_DATA_DIR`，副本 `$OKHUMAN_DATA_DIR`）。`main.go` 会在 `data.dir` 不以 `-<端口>` 结尾时自动补端口后缀 → 改 `server.port` 不必手改 data.dir。
- 起任何服务前先 `ss -ltnp`（一次给出端口+pid+可执行路径）+ `pgrep -f` 确认没有旧进程。`systemctl --user list-units --all '<前缀>-*'` + `pgrep` 先确认没有旧进程。只有临时实例（8454、8082）才用 `systemd-run --user --unit=<名> --collect …`。

## 8081（ninfer）

```bash
D=<本地盘>/kvmem-ninfer-v100
ss -ltnp | awk '/:8081 /{print}' | grep -oE 'pid=[0-9]+'   # 按端口取 pid；❌ 别用 pgrep -f "…/apps/ninfer-serve"，会连别的实例一起杀（2026-11-07 犯过两次）
systemctl --user start ninfer-8081    # unit 的 ExecStart 就是 start-8081-gsq.sh，日志仍 append 到 serve-8081-gsq.log；装载 15 秒 listening（页缓存热）、GPU2 占 14144 MiB
# 8082（临时、不自启）：systemd-run --user --unit=ninfer-8082 --collect --property=StandardOutput=append:$D/serve-8082-orca.log bash $D/start-8082-orca.sh（参数在 $D/conf-8082.env）
```
- **启动到 `listening`：权重在页缓存里时 13 秒**（2026-11-08 实测 15:30:18 starting engine → 15:30:31 listening），冷读盘约 40 秒（2026-11-07 实测：orca-iq3xxs 权重 9.29 GiB 按 ~1.0 GiB/s，17.8s 到 engine ready；原 gsq-rco 是 10.9 GiB）。2026-11-07 起 8081/8082 的模型都是 `models/orca-iq3xxs.ninfer`，`--model-id` 保持不变（8081=qwen3.8-27b，8082=qwen3.8-27b-orca），`warmup complete 441 ms`，KV/host cache 8 GiB。占 **卡 2 共 ~14 GiB**（V100-16GB 上余量不足 2 GB，别再往这张卡塞别的东西）。就绪判据 = 日志出现 `listening on http://127.0.0.1:8081`，或 `curl -s 127.0.0.1:8081/v1/models` 返回 `id":"qwen3.8-27b","context_length":200000`。
- 它启动时那条 `ECC is ENABLED on Tesla V100…disable it with nvidia-smi -e 0` 的警告**别照着做**：文案是按 RTX 3090（GDDR6X）写的，V100 是 HBM2、ECC 关不掉；照做只会白折腾一次重启。
- **停它用 `systemctl --user stop ninfer-8081`**（等价 `kill -TERM <pid of ninfer-serve>`）：约 6 秒内退出、显存全释放（那张卡回到 8 MiB），日志留 `stopping | running and queued requests are cancelled`。
- **要临时腾卡（不用停进程）：驱动级 checkpoint**（本机驱动 580.178.04，`libcuda` 有那六个 `cuCheckpointProcess*`；已做成 **`gpu-window` 插件**：`map` 按显存归属取 pid、`window` 让卡→做事→必换回、`protected` 挡住 8080、`gates` 用客户实例 /status.running 判断空闲）。**取 pid 只能按显存归属**：`nvidia-smi --query-compute-apps=gpu_uuid,pid,used_memory` + `--query-gpu=index,uuid` 对照——多进程服务打在 MainPID 上会"成功但无效"（8080 的 python 主进程几乎不占显存，真持有者是 `build-ab/strata`(12000 MiB) 与 `build-vision-cuda/bin/strata-vision`(1856 MiB)；对主进程 yield 0.045 s 返回 CHECKPOINTED 而显纹丝不动）。**8080 打在真持有者上同样能换**：GPU1 13984→5 MiB、7 s 让出、3.7 s 恢复、日志无异常（主人 2026-11-08 定：**不要动 8080**，要用只动 8081）。对 8081 实测：`yield` **5.7 s** → GPU 从 14142 MiB 降到 **5 MiB**、进程存活、`state=2=CHECKPOINTED`（枚举是 RUNNING0/LOCKED1/CHECKPOINTED2/FAILED3，2 不是错误）；`resume` **3.1 s** 回到 14142、下一条请求 TTFT 234 ms 正常。**KV 跨换出完好**：778 token 的同一请求换过一轮后 `cached=773`（99.4%）。代价与坑：让卡窗口内真请求会挂到超时（一条排 40.2s 后 cancelled），被换出进程的 **CPU 线程照常运行**（读文件、打日志都行），但任何触碰显存数据的调用会阻塞 → **`/v1/models` 仍返回 200 而真生成请求挂到超时**：别拿探活当可用判据，切换只在空闲时做。多服务分时也已实测（同一张卡上 A=8081 ↔ B=torch 6 GiB 互换两次：中途两服务都在内存、卡上只剩 5 MiB；换回的一方立即 200）。
- 默认思考档位 `--default-reasoning-effort xhigh`：旧版 build 就支持（`serve_options.cpp` 解析），但对我们这些模型**只等于"默认开思考"**——实测 minimal→xhigh 的 reasoning_tokens 169/151/180/117/123 无单调变化（模型模板不读 `reasoning_effort`、词表无 `<|think_*|>`）；真正控制思考量的只有 `--default-thinking-budget`。⚠️ 服务端默认档位会按 unset 记入请求日志（`translate.cpp`），别用日志判断它没生效。
- 脚本内含且别在命令行重复给：`CUDA_DEVICE_ORDER=PCI_BUS_ID`、`CUDA_VISIBLE_DEVICES=2`、`LD_LIBRARY_PATH=/usr/local/cuda-12.8/lib64`、`--model-id qwen3.8-27b`、`--default-thinking-budget 100000`、`--max-context 200000 --kv-capacity 200000 --kv-dtype rk4v4 --kv-lease-growth`、`--prefill-chunk 256 --vision --vision-max-merged 2048`
- 二进制 `kvmem/backends/ninfer/build-sm70/apps/ninfer-serve`（**sm70 = V100 专用构建**）。权重由 `convert-gsq-iq3s.sh` 生成（`mode_gguf/qwen3.8/27b/…-IQ3_S.gguf` + `mmproj-…-BF16.gguf`，`--recipe qwen3_8_27b_gguf --components text,vision`，byte-for-byte 不重量化；转换占 `CUDA_VISIBLE_DEVICES=2`）
- **视觉 2026-11-07 起常驻显存**（`--vision`，非 `--vision-cpu`）：多占 ~446 MiB，单实例总显存 13.68→**14.13 GiB**/16；1080p ≈ **2040 merged token**（实测 prompt_tokens=2104），故 `--vision-max-merged 2048`，2560×1440≈3600 才要 4096。同一张 1080p 首图：CPU 视觉 93.2s、GPU 视觉 9.0s（重复 1.6s） → 4096 ≈ 2048×2048 是这条的修法
- **脚本自己不重定向日志**，上面那行重定向就是 `serve-8081-gsq.log` 的来源；日志别落 tmpfs
- **2026-11-08 起它是 `ninfer-8081.service`（enabled，开机自启）**——此前「不做单元」是 2026-10-29/11-05 主人的决定，已由主人本人推翻，别再拿旧决定拦这件事。unit 里带 `ExecStartPre=wait-for-paths.sh`：盘没挂它会等着，别判成失败。（07:14 那次真实请求 prompt 110,730、cache 99.9%、decode 33.7 tok/s，07:31:56 停）。

## 8452（小B）与 8454（小G）

```bash
systemctl --user start okhuman-8452   # 6 秒内 listening；unit 里 WorkingDirectory 已写死（config/ 与 prompts/ 走相对路径）
```
- 实测（2026-10-29 08:59）：起后 `GET /health` 返回 `{"ok":true,"llm":"http://127.0.0.1:8081/v1"}`，日志自证 `data=$OKHUMAN_DATA_DIR`、`prompt=prompts/（2754 字符）`。**端到端** `POST /chat` 走通：`reply` 正常返回，8081 侧 `req#1 done | prompt 2,465 | output 34 | TTFT 5.3s | total 6.2s | prefill 521 tok/s | decode 40.2 tok/s | cache 0%`（冷会话 cache 0% 属正常，别当成故障）。
- 停：`systemctl --user stop okhuman-8452`。停完 `ss -ltnp | grep -E '8452|8081'` 应为空。
- **它起不来先查 8081**：判定顺序固定是 `curl -s 127.0.0.1:8081/v1/models` → 再 `curl -s 127.0.0.1:8452/health`。8081 不在时 8452 进程能起、`/health` 会 ok，但每轮 LLM 调用直接失败——`is-active`/进程在不在都不能证明它能干活。
- 源码与生产必须一致，同步用 `oktree-sync`（⚠️ 8454 在 `mode` 盘，**不在** `okp-sync`/`oktree-sync` 覆盖内，改生产提示词不会传过去）（排除 .git/config/01-identity/README 四处合法分歧）；`prompts/` 与 `config/` 各自独立，不跟搬。
- 不在自启内：8453、8454、okmon(8496)、cron(8601) → 见 `autostart.md`。
- **8454（小G）**：树 `<模型盘>/OkHuman-8454`（从 8452 复制），`data.dir=$OKHUMAN_DATA_DIR/okhuman-8454`，LLM = **:8082**（orca-iq3xxs），模型 id `qwen3.8-27b-orca`。**2026-11-08 主人定：8454 不要了** → QQ 桥侧已摘（`config.json` 删 instance `xiaoc` + bot `1906334924`，那两项 11-07 只改了工作区从未提交，所以摘掉后 git 里干净、无痕迹可查）；`.1` 上 `okhuman-entries` 的入口 18454 也删了（改 `UP` 表 + `sudo systemctl restart okhuman-entries`）。8082 与那棵树仍在盘上，要起仍按 8452 那行起。

## 探活与卡

- OkHuman：`curl -s 127.0.0.1:<port>/health`。两个推理后端都给 OpenAI 兼容路径：`curl -s 127.0.0.1:<port>/v1/models`。
- 卡分配：index 0 = GTX 750 Ti（不参与）；**8080 = strata config 里的 `gpu: 1`**；**8081 = `CUDA_VISIBLE_DEVICES=2`**（2026-11-08 从 3 改到 2，原因见「本机 GPU 真名」）。要空卡做模型转换得先腾一张。`nvidia-smi` 的 index 与 torch 卡序不一致 → 见 `../../day_17/multi-gpu-torch/SKILL.md`。
- 起 8080 的等待预算按页缓存分档：`experts loaded 46.84 GiB` 冷读盘 **340–454 秒**（0.23–0.25 GiB/s，2026-11-05 实测），**页缓存热时只要 20–27 秒**（6.7 GiB/s，2026-11-10 四次实测）。unit `TimeoutStartSec=600`；重启一律走下面的脚本（它自己轮询到 `loaded`），别手工 `sleep` 后盲判成败。

## 另一台机器（另一台，192.168.1.3）差异（2026-11-04 实测，2026-11-17 复测路径与实例名）

本文其余部分写的是**生产机（192.168.1.2）**。在 另一台 上对应值：

- 仓库根：`~/hdd/plugins`（本文所有以 `~_p` 开头的路径，换根即可）。`/home/ey/桌面/plugins` 是 root 所有的空壳，只剩 `qq-channel/data/…/tokens`，**不是活的**——判据：scout 与三个 okhuman 进程的 cwd 都在 500g_soft（2026-11-17）
- 实例：小E 8451 运行目录 `~/hdd/OkHuman`、小C 8452 `~/hdd/OkHuman-8452`（单元 `WorkingDirectory`/`ExecStart` 就是这个，2026-11-17 从单元文件读出）。跨机派活走它的 lan-bridge `:8602`，peer 里必须填 `inst`
- 数据/会话目录：`$OKHUMAN_DATA_DIR`、`$OKHUMAN_DATA_DIR`——**与生产同名、不带实例名**（旧命名 `okhuman-xiaod-8452`、`okhuman-xiaof-8452-8451` 还在盘上，是残留，别照它判断哪个实例在用）
- 服务单元名：`okhuman-8451` `okhuman-8452` `lan-bridge-8451/8452` `strata-8080`(小E后端) `ninfer-8081`(小C后端)。**两个后端都 `enabled`，但 `.3` 的 `strata-8080` 每次开机必 crash loop 约一小时**：它的 config 里 `log=/tmp/strata-logs/…`，`/tmp` 是 tmpfs 开机即空，`serve.server` 只 `open(log,"a")` 不建父目录 → `FileNotFoundError` → exit 1，`RestartSec=15` 空转到有人 `mkdir` 为止（2026-11-08 实测：开机 14:36:44 秒退、248 次计数、15:42 才 loaded——**那次是 `.3` 上的小D自己手工 `mkdir /tmp/strata-logs` 救的**，目录属主 root；`ninfer-8081` 一次成功 `NRestarts=0`）。即：那边有人能自救，但不要指望自救，修脚本才是正解（已修）。**缺的是 `.3` 树上 `run-strata-0.2.10.sh` 里本机 11-08 已加的三行**（从 config 读 `log` 的目录 + `mkdir -p`；`.3` 那份停在 11-05 的旧版 `read -r PORT GPU`）。**2026-11-08 已补齐**：整文件按 diff 唯一差异搬运（两份现完全一致），备份 `/home/ey/桌面/strata-volta-0.2.10/run-strata-0.2.10.sh.bak-20261108-add-logdir`；验证用真脚本副本 + 临时 config 把 `log` 指到不存在的路径 → 目录被创建，且全程不重启真服务（`MainPID`/`NRestarts` 不变）。诊断口诀：**后端单元反复 exit 1 又与 GPU/显存无关时，先看它的 `log` 路径在不在 tmpfs 上** `okhuman-scout` `okmon` `okhuman-cron`
- **桌面**：`.3` 也是 XFCE4（`Xorg :1`，1920×1080，lightdm）→ 做 GUI 操作要 `DISPLAY=:1`（`X0` 套接字存在但打不开）；**没装 xfce4-screensaver**，不会锁屏。
- ComfyUI：`<另一台盘>/ComfyUI-0.37.0`，端口 **8189**（2026-11-17 该端口没在听=没在跑；细节见 `comfyui` 技能的 `port_to_peer.md`）

**另一台 的单元与本机不同名**（见上面「服务单元名」那条），包装脚本在 `/home/ey/bin/start-*.sh`（**unit 里的路径不能含空格**，故用无空格脚本绕开）。

## 8080 的版本现况（2026-11-05 更新）

- **本机 8080 = Strata 0.2.10**，由 user 单元 `strata-8080` 启动：`ExecStart=<本地盘>/strata-volta-0.2.10/run-strata-0.2.10.sh …/strata-262k.json`（旧写法备份 `~/.config/systemd/user/strata-8080.service.bak-2026-11-05-pre-0210`；0.1.44.1 树已删除，恢复要重装）。**一键重启：`./restart-strata-8080.sh [-n] [-t 秒]`**（`-n` 只看现状不重启，`-t` 加载上限默认 600，与 unit 一致）：报现状 → 先探 8451 有没有在跑请求 → `systemctl --user restart` → 轮询 `/v1/models` 到 `status.value:"loaded"` → 失败 tail 日志 → 新旧 `meta.n_ctx` 不同就提示配置已改。实测 2026-11-10 两次：35 秒 / 46 秒就绪（页缓存热）。**当前上下文 = 262144**（`args` 里 `--max-context`，与 `/v1/models` 的 `meta.n_ctx` 一致）；教训：`strata-262k.json` 11-10 07:30 被改成 262k，而 11-08 起的旧进程一直还在跑 512k → **改 config 不重启就不生效，判当前真值只看 `meta.n_ctx`**；要不要回 512k，主人未定。
- **采样：本机 config 里已不含 `sampling` 块**（2026-11-10 主人指示删，备份 `strata-262k.json.bak-20261110-drop-sampling`，原值 1.0/0.95/20）。取值链：模型卡默认 → config 块 → `/settings` shared → 请求字段（请求最优先，`serve/server.py:5887`）；一处都不给才 = 贪心（`req_temperature=0.0 → greedy`，`src/program/generate.cpp:15090/17140`）。**模型卡默认即使 config 没写 `arch` 也生效**（`serve/server.py:9846: svc.model_arch = cfg.get("arch") or "qwen4exp"` → `MODEL_CARD_SAMPLING` :9295 = thinking 1.0/0.95/20、instruct 0.7/0.8/20+presence 1.5）——旧笔记写的「没写 arch 就不生效」是错的，2026-11-10 更正。**判有没有 config 块**：`serve/server.py:9733` 会打 `sampling defaults from the config`，该次启动之后计数 0 = 没有（全文历史累计 76 次）；该版引擎不打 per-request 采样字段，别指望从请求日志看。删块后思考档参数与删前逐字同值 → 行为不变，只有非思考档会从 1.0/0.95/20 变成卡上的 0.7/0.8/20+presence 1.5。**OkHuman 客户端从不发请求级采样参数**（`internal/llm/client.go:4`：请求级下发会改模板渲染、破 KV 缓存，A/B 实测坐实）→ 要改只能改 config 块。实测 2026-11-07：两次相同请求的思考内容逐字不同 ⇒ 在采样而非贪心（`.3` 的 8080 同版本同参数，也在采样；升级做法见 `lan-ssh` §7.2b）。
- 起法：`run-strata-0.2.10.sh [config]`，port/gpu 从 config 读并显式传；它用本树自带的 `.venv`（119 MB，装 `requirements.txt`），**不需要旧 venv，也不需要复制 gguf-py**（运行时不 import gguf）。
- 实测（真实流量口径）：升级本身对 decode 基本无感（中位 34.1 → 34.4）；真正有用的是**会话缓存复用**（16.5 万 token 复用命中 2.7 秒 vs 全量重读 ~169 秒）。
- **视觉**：`vision.elastic=false` + `--vram-reserve-mib 2048` = 图像编码器常驻 GPU（日志出现 `warmed up at 4096 image tokens` 即生效，占 ~1.86 GiB 显存），代价是专家缓存 3275 → 2320 槽（每槽约 1.97 MiB）。不设这两项时，<23.5 GB 的卡默认退回 CPU 识图。
- 别同时起两个 262k 实例：两个都要往内存装 55 GB 专家权重并抢同一块盘，装载时间翻倍、双方都慢。

## 本机 GPU 真名（2026-11-06 查证，别再猜）

`GPU0 = NVIDIA GeForce GTX 750 Ti 4GB` —— **跑不了 H3 视频 / TTS / LLM**，别当空卡用；**V100 只剩两张**：`GPU1 = PCI 03:00.0`（strata 8080）、`GPU2 = PCI 04:00.0`（ninfer 8081）。
**2026-11-08 实测：第三张 V100 已从 PCI 消失**（`lspci -d 10de:` 只有 01:00.0/03:00.0/04:00.0）→ 写死 `CUDA_VISIBLE_DEVICES=3` 的脚本必 `cudaGetDeviceCount failed: cudaErrorNoDevice`（8081 那天起不来就是这个，与模型、驱动无关）；**起任何 GPU 服务前先 `nvidia-smi -L`**。⚠️ GPU2 原本是留给 ComfyUI(:8188) 的渲染卡，8081 常驻后余量不足 2 GB → 要出图先停 8081 或换卡。
选卡两步都不能省：① `nvidia-smi --query-gpu=index,name,memory.total` 先查型号；② `CUDA_DEVICE_ORDER=PCI_BUS_ID CUDA_VISIBLE_DEVICES=<index>`（不设它，CUDA 的 0 号是另一张卡，据此 OOM 过两次）。
起 ComfyUI 报 `SolAttn (V100) requires an SM70/V100 CUDA device` = 选到 750 Ti 了。
- **ASR（:8090）在两台都不是单元**：`.3` 上它是插件自带的 `bin/llama-server` 脱管进程（`pgrep -af llama-server` 才看得见），机器重启不会自启。要知道「谁占了哪个端口/哪张卡」，用 `lan-ssh/remctl.sh matrix`（含每张卡的显存归属单元）。
