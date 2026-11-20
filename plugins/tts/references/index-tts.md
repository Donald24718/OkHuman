# IndexTTS-2.5 — 零样本音色克隆（本机第二套 TTS）

> 不在本插件内，只是登记在 tts 能力面下（同域不另立门户）。
> 2026-10-25 本机实测验证：接口、耗时、显存、输出规格、以及一个会静默出乱语的坑。

## 在哪 / 是什么

`<模型盘>/index-tts`（独立 repo + 自带 `.venv`，与 OkHuman 无耦合）。
权重在 `checkpoints/`：`gpt.pth` 3.3G、`codec.pth` 607M、`s2mel.pth` 415M、`qwen0.6bemo4-merge/`（情感模型）。
`config.yaml` 里 `version: 2.5`。辅助模型（w2v-bert-2.0、semantic_codec、campplus、bigvgan）**已在 `checkpoints/hf_cache/` 下齐**，离线可跑，不用再联网。

能力：给一段参考音频即可零样本克隆音色；中/英/日/西/阿；情感（参考音频或文本描述）、语速（`duration_factor`）、拼音/CMU/假名发音控制。
GPU 上最快；**CPU 也能跑**（2026-10-25 实测，见下表），只是慢约 4 倍。与 Kokoro 的根本差别是：Kokoro 是亚秒级轻量，IndexTTS 模型大、加载 20 多秒。

## 坑（先读这条，否则会拿到乱语）

repo 里有**两个同名 `IndexTTS2` 类**，选错不报错、只出乱语：

| import | 对应模型 | 精度参数 | tokenizer |
|---|---|---|---|
| `indextts.infer_v2_5` | **2.5（本机这套权重）** | 只有 `use_bf16` | `multilingual_zh_ja_yue_char_del.tiktoken` |
| `indextts.infer_v2` | 2.0 | `use_fp16` | `checkpoints/bpe.model`（SentencePiece） |

2026-10-25 实测：用 2.5 权重 + `infer_v2`（2.0 类）能加载、能出音频，但**内容完全不对**
（输入"今天天气不错，我们出去走走" → ASR 回读"嗯，是的，是的，嗯"）。
`webui.py --version 2.5` 会自动 import `infer_v2_5`，所以 webui 是对的；**自己写脚本必须显式 `from indextts.infer_v2_5 import IndexTTS2`**。

顺带：2.5 的模型卡里**没有 `bpe.model`**（只有 2.0 卡有）。我 2026-10-25 从 `IndexTeam/IndexTTS-2` 补了一个（476 KB）进 `checkpoints/`，
只为让 2.0 那条路能加载；**跑 2.5 不需要它**。它的存在正是上面那个坑能"静默通过"的原因。

## 跑法（已验证）

```python
import sys; sys.path.insert(0, "<模型盘>/index-tts")
from indextts.infer_v2_5 import IndexTTS2          # ← 必须是 infer_v2_5
tts = IndexTTS2(cfg_path="checkpoints/config.yaml", model_dir="checkpoints",
                use_bf16=False, device="cuda:0")    # V100 无 bf16 单元 → 走 fp32
tts.infer(spk_audio_prompt="参考音频.wav", text="要说的话",
          output_path="out.wav", lang="zh")         # ← lang 在 2.5 是必填位置参数
```

从 repo 目录跑，并按 UUID 锁卡（torch 序号 ≠ nvidia-smi 序号，见 multi-gpu-torch skill）：

```bash
cd <模型盘>/index-tts
CUDA_VISIBLE_DEVICES=GPU-3673d0dd-1b7d-ddef-1f52-1a502f969dea \
  .venv/bin/python 脚本.py       # 该卡 = GPU2，平时留给 ComfyUI，用完停
```

图形界面 / 命令行工具：`.venv/bin/python webui.py --version 2.5 --port 7860`（`--fp16` 在 V100 上会退回 fp32，别加）；
另有 `indextts/cli_v2.py`。

## 实测（2026-10-25，GPU2 按 UUID 单卡暴露，fp32）

| 项 | 值 |
|---|---|
| 模型加载 | 26.9 s |
| 常驻显存 | 6.47 GiB allocated / 6.6 GiB reserved（合成后 6.71 / 7.58） |
| 14 字句 | 合成 4.8 s → 音频 2.79 s |
| 44 字句 | 合成 7.7 s → 音频 7.34 s |
| 输出 | 22050 Hz 单声道 wav |
| 参考音频 | 3.83 s（用 tts 插件 Kokoro 现合成的样本当参考）→ 零样本克隆成立 |
| 内容校验 | ASR（:8090）回读与输入文本一致 |

同一脚本改 `device="cpu"`（44 线程，2026-10-25）：

| 项 | 值 |
|---|---|
| 模型加载 | 23.6 s（**和 GPU 差不多**，瓶颈在读权重不在算） |
| 14 字句 | 合成 **20.1 s** → 音频 2.75 s |
| 44 字句 | 合成 **39.6 s** → 音频 7.19 s |
| 相对 GPU | 慢约 4 倍（GPU 4.8 s / 7.7 s）；RTF 5.5–7.3（比实时慢 5~7 倍） |
| 内容校验 | ASR 回读与 GPU 版逐字一致 |

`device="cpu"` 时代码会把 `use_bf16` 强制置 False（`infer_v2_5.py:97`），不用自己管精度。
卡被 ComfyUI/别的服务占着、或者只是离线批量出几条音，走 CPU 完全可行；要即时回话就别走它。

与 2026-10-17 那次记录（加载 21.5 s、常驻 6.5 GiB、9 字 4.4 s / 28 字 11.5 s）同量级，可互相印证。
显存差异的来源是 `use_qwen_emo`：2.5 默认 **False**（6.5 GiB），2.0 的类默认 **True**（实测多到 8.2 GiB）。
10-17 那次的比值是 2.8 倍（两句：GPU 37.5 s / CPU 104 s），本次 4 倍——文本与当时负载不同，结论一致：**CPU 可用，慢数倍**。

## 什么时候用它，什么时候用 Kokoro

| | Kokoro（本插件，:8092） | IndexTTS-2.5 |
|---|---|---|
| 音色 | 固定 `zf_xiaoyi` | **任意参考音频克隆** |
| 资源 | CPU 就够（RTF≈0.35） | GPU 一张 V100（6.5 GiB）或 CPU 都行；加载 20+ s |
| 首句延迟 | 亚秒级 | 加载 24~27 s + 每句数秒（GPU）/ 二十到四十秒（CPU） |
| 情感/语速控制 | 无 | 有（参考音频 / 文本 / `duration_factor`） |
| 适合 | 朗读、播报、快速出音 | 要"某个人的声音"、要情绪 |

要克隆音色才起它；只是念文本用本插件。用 GPU 时用完记得停进程，GPU2 是留给 ComfyUI 的；不急着出音就直接 `device="cpu"`，不占卡。
多卡/显存/精度的通用坑见 `OkHuman_skill/year_2026/month_10/day_17/multi-gpu-torch/SKILL.md`。

## 选参考音频：### 别用自估基频判男女（自写 F0 判定不可靠）
自写的自相关/YIN 估基频并按 150 Hz 划界**不可作判据**（阈值与归一化都不稳，会把男声判成女声）。按下面的分类器判，并按来源身份佐证。

### 性别判定：用现成分类器，别用自估基频（2026-11-06 实测）
- 模型：`<模型盘>/gender-test/`（JaesungHuh/voice-gender-classifier，ECAPA-TDNN 20.8M，VoxCeleb2 微调，MIT）。CPU 跑，10 条音频含加载约 3 秒（每条 ~0.1 s）。
- 调用（**不要直接喂非 16k 的文件给它自带的 quick_check.py**，那条重采样会升成 float64，报 `expected scalar type Double but found Float`；正确做法是自己先转）：
  `ffmpeg -i x.wav -f f32le -ac 1 -ar 16000 - ` → `np.frombuffer(...,dtype=np.float32)` → `torch.from_numpy` → `softmax`，判 `p[1]>p[0]`。
- 我这边的真值集（靠朗读者身份与主人耳朵确认）：**13/13 正确，置信 97–99.8%**；含 TTS 合成成品、2.1 s 短句、老年男声（单田芳）。3 秒切片方向稳定（最低 90.9%）。混合两个说话人时置信降到 84%（可当"音频不干净"的信号）。
- 它抓出过我自己的错：上一片当"女说书人"用的参考音频，它判 male 99.2%（实为男声）。
- 用法（2026-11-06 主人定）：**默认以本分类器定案，不找主人听**。置信 ≥0.9 就写入音色表继续往下做。
- 边界与升级条件（这才需要人工）：模型在 VoxCeleb（全英文）训练，`RESULTS.md` 记录中文 AIShell 86–91% 且**有男声被高置信误判为女声**；因此只在三种情况下升级——置信 <0.9、不同窗口结论相反、或与**已知来源身份**冲突。升级先切片复测（3 秒窗方向稳定），仍冲突才请主人听一次。来源身份仍写进音色表当佐证，但不是必需的人工步骤。
- 实测结案例（2026-11-06）：`候选女声A_北京春节_7s.mp3` → male 99.8%，已改名 `已判男声_北京春节_7s.mp3`（没有找主人听）；两部片实际用的女声参考 `BV1effiYLEFJ` → female 99.4–99.5% ✓。
- 它**不回答**"两个角色音色是否太像"——那仍用 IndexTTS 说话人嵌入两两余弦（>0.5 判太接近）。

## 两机对照（2026-11-06 实测）

| | 本机（.2） | 另一台（.3） |
|---|---|---|
| IndexTTS-2.5 | `<模型盘>/index-tts`（checkpoints 11 G + `.venv` 8.3 G） | **已可用**：`/home/ey/mode_gguf/index-tts`（代码+checkpoints+整个 `.venv` 直搬）。拦路石是它要 **Python 3.11**（`.3` 只有 3.12，apt 装不到 3.11）——解法：搬 **uv 自带的独立 CPython** `~/.local/share/uv/python/cpython-3.11.13-linux-x86_64-gnu`（101 MB，自带 stdlib）到对端同路径，再改 `.venv/pyvenv.cfg` 的 `home=` 与 `.venv/bin/python*` 三个符号链接 |
| 性别分类器 | `<模型盘>/gender-test`，用自带 `venv/bin/python quick_check.py <wav…>` | **`/home/ey/mode_gguf/gender-test`（已放，权重 59 MB）**；**不用搬 venv**——借它的 ComfyUI venv：`/home/ey/桌面/ComfyUI-0.37.0/venv/bin/python`（torch 2.13 + safetensors 已够）。实测同一 wav 两台结论一致（male 99.8%）。`persian-hubert/` 用不到 |
| Kokoro（本插件） | 本插件 venv，CPU | 插件目录同构已同步；venv 未搬，要用时按 `.venv`→`lib/python3.12/site-packages` 直搬法（见 `comfyui/port_to_peer.md`） |

- 上面这套在 `.3` 上实测通过：`.venv/bin/python` = 3.11.13，`torch 2.8.0+cu128`、`cuda.is_available()=True`；
  **CPU** 冒烟 load 17.5 s、合成 4.4 s 音频用 23.3 s，产物送回 `.2` 用 ASR 逐字读回正确。要在 GPU 上跑得先腾卡（它两张卡被 `strata-8080`/`ninfer-8081` 占满）。
- **`indextts` 不是 pip 装的包**，是靠仓库根在 `sys.path` 里 import 的 → 跨机跑必须 `PYTHONPATH=<仓库根>`（或脚本里 `sys.path.insert`）。
  把脚本放在 `/tmp` 跑会 `ModuleNotFoundError: No module named 'indextts'`（`sys.path[0]` 变成 `/tmp`）。
- 两台的仓库根/模型根对照见 `host-crash-autostart/services.md`「另一台机器」一节。
- `tts_batch.py` 里 `sys.path.insert` 与 `os.chdir` 写死了 `<模型盘>/index-tts` → 换机器要改这两行（或走环境变量）。
