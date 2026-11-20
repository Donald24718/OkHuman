## TTS（Kokoro-82M，本地）

Kokoro 中文音色（8 个，config.json `kokoro_voice` 指定，**当前 zf_xiaoyi 晓依**，标准普通话女声；晓北偏东北/北京腔）：男 zm_yunjian/yunxi/yunxia/yunyang，女 zf_xiaobei/xiaoxiao/xiaoyi/xiaoni。

- serve：由 **tts 插件**提供（`tts/serve.py` + `tts/venv/`，stdin 一行文本 → outdir 出 wav，模型常驻），CPU RTF≈0.2~0.8（比实时快，段间预取合成不空转）。voice_loop 用 `config.json` 的 `tts_dir` 定位，默认取同级 `../tts`。
- 英文：v1.0 中文管线默认删掉英文段，serve 里包了一层 G2P——拉丁字母段经 espeak IPA 转换后正常朗读（2026-09-16 加，发音偏中式英语腔属正常）
- 播放：常驻 aplay 进程读 stdin 管道（单条连续音频流，段间零间隙零重叠；`aplay -q -D pipewire`）。2026-09-16 两轮实测复盘：① aplay 播完一段 wav 即正常退出（"常驻"是假象，实际每段重起），尾部缓冲竞态会 BrokenPipe（318 段 75 次）但自动恢复、听感可接受；② 曾改「每段一次性 aplay」→ aplay 退出时 PipeWire 缓冲尾部还在播，段段边界重叠，用户听到回音 → 已恢复常驻单流。防双播：mark_broadcast() 返回「是否新打标」，语音管道与全量播报轮询器都只在首次见到某条回复时入队（2026-09-16 修：原竞态会同一句播两遍）。模型/音色首次运行自动从 hf-mirror 下载（缓存在 ~/.cache/huggingface）
- 换音色：改 `kokoro_voice` 后重启 voice_loop

## TSE 目标说话人提取（2026-09-16 已删除）

SpEx+ 声纹分离（129M 模型 + serve + 代码）已于 2026-09-16 清理：宿舍实战效果不如预期，纯声纹门控已够用。若将来需要"剔除同时说话的他人声音"再重建（会话存档里有完整实现记录）。
