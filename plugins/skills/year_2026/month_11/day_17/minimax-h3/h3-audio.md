# H3 的声音：Music3 配乐、混音、同期声、响度

## 八、音乐与混音

Music3：`minimax_music3_dit_int8_convrot` + `SaveAudioMP3`（别用 `SaveAudioAdvanced`）。
150 s 的歌在单张 V100 上 ~32 min（AR 3751 步 ≈30 min + 分块 VAE 解码），别按短曲外推。
BGM 垫台词：`volume=-8dB` + `sidechaincompress=threshold=0.04:ratio=5`，`-c:v copy` 只重混音频。

## 十一、H3 会生成同期声；成片音轨必须显式 map（2026-10-27 实测）

- **H3 是音视频一体的模型**：提示词里写了 `overall_soundscape`，输出的 mp4 **自带音轨**（风、纸页、湿泥、石厅混响都真在）。
  所以做成片时"片内环境音"这一床是现成的，垫 `volume=0.13` 即可，不要以为素材是静音。
- **多输入合成必须 `-map 0:v -map 1:a`**：素材自带音轨时，`ffmpeg -i 视频 -i 混音 ...` 不写 map 会**自动选 input 0 的音轨**，
  做好的混音根本没进成片（症状：成片响度和混音文件实测差 2 dB 以上，逐段 RMS 与 video_raw 一致而与 mix.wav 不一致）。
- **响度要两遍 loudnorm**：单遍是动态模式，实测目标 -14 会落到 -16.4。
  第一遍 `loudnorm=I=-14:TP=-1.5:LRA=11:print_format=json -f null -` 取 JSON，
  第二遍带 `measured_I/measured_TP/measured_LRA/measured_thresh/offset + linear=true`。
  **这版 ffmpeg 的 JSON 键名是 `input_i/input_tp/input_lra/input_thresh`**，不是 `measured_*`，解析要两种都兼容。
