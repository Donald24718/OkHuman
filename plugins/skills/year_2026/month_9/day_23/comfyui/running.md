# ComfyUI 本机：起服务、节点、模型、API、交付

## 一、启动（照抄，别改）

```bash
cd "<本地盘>/ComfyUI-0.37.0" && \
# 先查型号再用！本机 GPU0 是 750Ti(4GB)，跑 H3 会报 SolAttn 错
nvidia-smi --query-gpu=index,name,memory.total --format=csv
systemd-run --user --unit=comfyui-8188 --collect \
  --working-directory="$C" --setenv=CUDA_DEVICE_ORDER=PCI_BUS_ID --setenv=CUDA_VISIBLE_DEVICES=2 \
  --property=StandardOutput=append:~/logs/comfyui-8188.log --property=StandardError=append:~/logs/comfyui-8188.log \
  "$C/venv/bin/python" main.py --listen 127.0.0.1 --port 8188 --disable-comfy-compiler --cache-lru 40
```

- `CUDA_DEVICE_ORDER=PCI_BUS_ID` **必带**：不带时 CUDA 枚举 ≠ nvidia-smi index，会挑到被占的卡。
- `--disable-comfy-compiler` **必带**：不带时 aimdo 内存预取对 FP16Safe 报 `aimdo memory compile error`，采样第一步即挂。
- 钉 GPU2（其余卡被 8080/8081 的 LLM 占着）。起完核对：`ss -ltnp | grep 8188` 取 pid，`readlink /proc/<pid>/cwd` 应是 ComfyUI 目录。
- **别用裸 `setsid … &`**：它只换会话不换 cgroup，会挂在调用方（bash 工具或临时单元）的单元里被连坐杀掉；`--unit=comfyui-8188` 才独立。**可执行路径必须绝对**——systemd 不解析 `./venv/bin/python`（报 `Failed to find executable`）。`--listen 127.0.1.1` 起的那个在 gpu-window 里是 `[未登记]`，要按 pid 操作。
- 改管线/加节点后要**重启**才生效。停：确认 queue 空 → `kill -TERM`。

## 二、V100 加速：只有 SolAttnV100（~7.5×）

工作流里挂在 UNETLoader 与 Guider 之间：

```
UNETLoader(minimax_h3_fl2va_pruned_fp8_scaled) → SolAttnV100 → BasicGuider / BasicScheduler
SolAttnV100: fp16_safe=true, tau=0.75, start_percent=0.2, end_percent=0.9,
             min_tokens=1024, dense_blocks="0-1,-1", h3_prefix_tokens=1024, topk_blocks=32
```

参数用默认（`h3_prefix_tokens` 别改成 ≈S，末尾帧会崩）。生效标志（日志）：
`[SolAttn][V1.1.1] active: ...`。实测 960×544×124 帧 20 步 ~28 s/步（裸跑 ~210 s/步）。

**别试这些，V100(sm_70) 上全部无效**：
- `--use-sage-attention`：sageattention 只编了 sm_89 内核 → 报 `Unsupported CUDA architecture: sm70` 后静默退回 pytorch。
- `--enable-triton-backend`：H3 的 fp8 路径报 `fp8e4nv not supported in this architecture`（Volta 只有 fp8e4b15/fp8e5）。comfy_kitchen 0.2.35 不带开关时 triton 也显示 `disabled:False`，仍会撞这个错 → 需要时显式 `--disable-triton-backend`。
- comfy-kitchen cuda 后端：被 `torch.version.cuda < 13` 硬禁（本机 torch 2.13.0+**cu126**；`nvidia.cu13` 包在也没用）。
- 4 步蒸馏（turbo_4step LoRA / SLAV100）：主人明确不用。


## 三、模型名（现存，写错就报缺模型）

```
diffusion_models/minimax_h3_fl2va_pruned_fp8_scaled.safetensors  # FL：T2V/I2V/首尾帧，20G（2026-10-24 起默认）
diffusion_models/minimax_h3_ref2va_pruned_fp8_scaled.safetensors # RE：R2V 参考图，20G（2026-10-24 起默认）
# **bf16 已于 2026-10-24 彻底删除**（软链接与实体都没了，各 38G）→ 想做 fp8 vs bf16 对比必须重新下载 80G
# fp8_scaled 在 V100 走 emulated ops（日志 `emulated ops: ... float8_e4m3fn`）：省显存/磁盘，不省算力
text_encoders/qwen3-vl-32b-minimax-h3-fp8-comfyui.safetensors  # CLIPLoader type="minimax"
vae/minimax_h3_video_vae_fp16.safetensors                      # 视频 VAE
vae/minimax_h3_audio_vae_fp32.safetensors                      # 音频 VAE（R2V 要，I2V 没有该输入）
diffusion_models/minimax_music3_dit_int8_convrot.safetensors   # Music3
```
**int8/bf16 与 Qwen-Image-2.1 已全部删除**（2026-10-24）→ **H3 只有上面两个 fp8_scaled**；要做 fp8 vs bf16 对比必须重下 80 G。
实测（2026-10-24，fp8 + SolAttnV100，960×544×124 帧 20 步）：**26.9 s/it，全程 573 s**；736×416×60 帧：**7.62 s/it**。

## 四、路径

- 输出：`<本地盘>/ComfyUI-0.37.0/output/<filename_prefix>`
- **LoadImage 只认 `input/` 里的 basename** → 外部图先拷进去（用 output 路径会 400）。


## 五、API 三步

```python
pid = json.loads(urllib.request.urlopen(urllib.request.Request(
    "http://127.0.0.1:8188/prompt",
    data=json.dumps({"prompt": graph, "client_id": "x"}).encode(),
    headers={"Content-Type": "application/json"})).read())["prompt_id"]
# node_errors 非空 = 校验失败，读 body 定位缺哪个字段
json.loads(urllib.request.urlopen(f"http://127.0.0.1:8188/history/{pid}").read())   # 进度/结果
# outputs[node]["videos"|"images"|"audios"] → filename/subfolder
```
`GET /object_info/<Node>` 查节点签名，**别猜参数**。

必踩的五条：① `SaveVideo` 必须给 `codec`（如 `"auto"`），否则跑完 30 min 才崩；
①b **`SaveVideo` 的 `format` 也必须给，且合法 key 是 `"auto"`**（不是 `video/h264-mp4`——传错值会被
   dynamic combo 静默丢掉，报 `SaveVideo.execute() missing 1 required positional argument: 'format'`，
   而且是**采样全跑完之后**才报；改一个字段重提交，前面节点命中 LRU 缓存，1.8 s 出片）；
①c **纯文生视频（T2V）**：本机没有本地 T2V 节点（只有云端 `ComfyCloudMiniMaxH3TextToVideoNode`），
   用 `MiniMaxH3ImageToVideo` **不传 first_frame/last_frame** 即是 T2V（两者都是 optional）；
② `MiniMaxH3ImageToVideo`/`ReferenceToVideo` 的 **LATENT 是输出 [1]**，[0] 是 CONDITIONING；
③ 队列空 ≠ 失败（history 落盘有几秒延迟），宽限 90 s 再判死；
④ 官方 workflow JSON 是 UI 格式（`nodes[]`），API 提交要展开成 `{node_id:{class_type,inputs}}`。

## 九、交付

QQ 桥单文件 **4.5 MB 上限**：超了先压（视频 `-b:v 570k -b:a 128k`；音频 `-b:a 160k`），原片留盘；投递入口与类型映射见 qq-channel README（`okhuman-qq-bridge attach <文件>`）。

- **显存**：H3 跑起来 GPU2 常驻 15.1–15.9 / 16.4 GB，**同一批里不能再往 GPU2 塞第二个模型**
  （Music3 必须排在视频批之后）。GPU2 平时被 8081（kvmem-ninfer）占 ~15.4 G → 渲染前先让卡：
  `gpu-window yield 8081 --cmd <渲染>`（成功即自动换回）。**对 8081 别用 `gpu-window resume`**——它走
  `kvmem-ctl restore`，对 kvmem-ninfer 必然报 `restore: OS call failed`（见 gpu-window README，2026-11-14）；
  恢复只能 `systemctl --user restart ninfer-8081`（装载约 1 分钟），且**先停占卡的 comfyui 单元再 restart**，否则它起不来、停在 failed（2026-11-14 实测：journal 无 OOM 字样，停 comfy 后即 200）。
