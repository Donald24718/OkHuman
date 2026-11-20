# 跨机使用 / 移植 ComfyUI（2026-11-04 实测，生产 .2 ↔ 另一台 .3）

## 一、两台机器的事实（照这个填参数，别猜）

| | 生产（本机，192.168.1.2） | 另一台（192.168.1.3） |
|---|---|---|
| ComfyUI 目录 | `<本地盘>/ComfyUI-0.37.0` | `/home/ey/桌面/ComfyUI-0.37.0` |
| Python / torch | 3.12.3 / **torch 2.13.0+cu126** | 同（site-packages 直接搬，见下） |
| 端口 | 8188（只绑 127.0.0.1） | **8189**（8188 留给生产，别撞 LLM） |
| GPU | 3×V100-16G，H3 用 **GPU2** | 2×V100-16G，**平时被 `strata-8080`(15.4G) + `ninfer-8081`(15.9G) 占满** |
| 模型 | 实体在 `<模型盘>/…`，models/ 下是**符号链接** | 权重在 `models/diffusion_models`(46 G)/`text_encoders`(34 G)/`vae`(6 G)/`loras`(1.2 G)，共 87 G，**不用重搬**；`models/unet` 为空是正常布局（H3 权重不在 unet 目录）。我 11-06 曾据此误判成「模型已清空」。 |
| 启动参数 | `--disable-comfy-compiler` + 图里挂 `SolAttnV100` | 完全相同；腾出整卡时**不需要** `--lowvram` |

**谁是执行机都一样**：两台互为对等（`.3` 也能当发起端）。执行前先 `lan-ssh/remctl.sh matrix` 确认哪张卡空着；两台的单元名对照见 `host-crash-autostart/services.md`。

## 二、`.3` 上跑之前必须先腾一张卡

**先查谁占着那张卡，再决定停哪个**（别照下面的清单顺手停）：`lan-ssh/remctl.sh matrix ey` 或 `remctl.sh owns <端口> ey`
会列出每张卡的显存归属单元。2026-11-06 我停的是 `ninfer-8081`，而我要的那张卡其实被 `strata-8080` 占着 → 白等一轮。

- 报错 `aimdo: VRAM grow failed 1.55GB` 的真因通常就是**目标卡被别的单元占着**，不是显存参数问题；
  `--lowvram` 在默认 dynamic VRAM 下**不起作用**，别拿它当解法。
- 这些 LLM 后端在 `.3` 上都是 **root 跑的 system 单元**：`pkill` 打不到（报「不允许的操作」），必须 `sudo systemctl stop|start`。

```
sudo systemctl stop ninfer-8081      # 小F后端（占 GPU0 15.4G）
# 或 stop strata-8080（小D后端，占 GPU1 15.9G）—— 代价：那个实例这段时间没有 LLM
```
跑完复位并**验收**（不是"start 了就算"）：`systemctl is-active` + `.3` 本地 `curl 127.0.0.1:8081/v1/models` = **200**；
再从 `.3` 本地探 `127.0.0.1:8452/health` = 200。我从局域网探 8081 得 000 —— 因为它只绑 127.0.0.1，属正常。

## 三、移植流程（实测约 25 分钟，不下载模型）

1. **同步代码**：`rsync -a --exclude='/venv' --exclude='/models' --exclude='/input' --exclude='/output' src/ ey:DST/`
   —— **排除必须带前导 `/` 锚定**。不锚定的 `--exclude models` 会把 `comfy/ldm/models` 这类**代码目录**也排除掉，
   症状：启动时 `ModuleNotFoundError: No module named 'comfy.ldm.models'`。
2. **模型用 `rsync -aL`**：我这边的模型文件是符号链接，不解引用过去全是断链（`.3` 上验证过：断链数 0）。
3. **venv 别在对面 pip 装**（`.3` 到 pypi/阿里云只有 0.1–0.45 MB/s）。直接
   `rsync -a venv/lib/python3.12/site-packages/ ey:DST/venv/lib/python3.12/site-packages/`
   —— 两边同为 x86_64 + Python 3.12 + V100(sm70) → 可直接用，8.3G 局域网 **35 秒**（对比 pip 装 40+ 分钟）。
4. **对面要装的系统包**：`python3.12-venv`（建 venv 要 ensurepip）、**`python3.12-dev`**
   —— 后者是硬坑：H3 的文本编码器路径会在运行期 JIT 编译 `cuda_utils.c`，缺 `Python.h` 会在 `MiniMaxH3ReferenceToVideo` 节点报
   `subprocess.CalledProcessError`（完整原因是 `fatal error: Python.h`，被截断很容易看漏）。
5. **`SolAttnV100` 的 `.so` 不用重编**：它绑 cp312 + torch2.13.0 + sm70，对面 python/torch 同版本就直接可用。
   注意：单独 `import` 那个 `.so` 会报 `libc10.so: cannot open shared object` —— **那是测试写法错**，先 `import torch` 再 import 就通过。
6. **冒烟测试**（先别上正式尺寸）：`ReferenceToVideo` 512×512、`length=22`、`steps=8` → `.3` 上 **13 秒出 22 帧**，
   峰值显存 ~11.5G（16G 卡够用）。通过再跑正式尺寸。
7. **复位**：停掉 ComfyUI，把停掉的 LLM 后端 `start` 回来并按第二节验收。

## 四、跨机执行命令的两个坑

- `ssh ey 'pkill -f "…8189"'` 会**匹配到正在执行它的远端 shell 自己**，把 ssh 会话杀掉（表现：`exit 255`）。
  用 `p=$(pgrep -f "[m]ain.py.*8189"); [ -n "$p" ] && kill -TERM $p`。
- 远端要常驻的进程用**脚本文件 + 三件套脱离**：`setsid bash /tmp/x.sh >/tmp/x.log 2>&1 < /dev/null &`；
  直接在 `ssh '…&'` 里起，ssh 一断进程就没了。

## 五、两台并行渲染（2026-11-06，《尸变》《查牙山洞》30 镜实测）

- **开跑前先确认远端也有脚本与 json**，不只是工作流 json：我两次漏拷 `gen_shots.py` / `shots.json`，对面空转 25 分钟。
  `remctl.sh pre ey <远端路径…>` 就是查这个。
- **产物按 mtime 从两台收**，别指望某个目录是全集：base 在 `output/shot`，续接段在 `output/ext`（前缀 `Sxx_xN`）。
- **跨机拼同一镜头的续接段要反向 rsync base**：A 机渲的 base，B 机续接时必须先有它。
- **同时跑的实例越多单段越慢**（多实例从主机内存流权重互相拖）：实测单段 400 s → 1300 s。三线并行的收益要先减掉这一项。
