## 架构控制台 WebUI

`http://127.0.0.1:8495/`（随 start.sh 一起启动，纯 stdlib 无依赖）：

- **状态总览**：主循环 / TTS / 声纹门控 / ASR / 暂停状态，3 秒自动刷新
- **改配置**：改任意配置项 → "保存并应用"（校验后写 config.json 并重启 voice_loop，
  约 10s TTS 模型重载）；只保存不重启需手动
- **暂停/恢复**：暂停 = 麦克风输入丢弃（agent/TTS 服务保持热备，恢复零等待）；
  实现是 `data/paused` 状态文件，voice_loop 每轮检查
- 日志尾部实时展示（/tmp/voice_loop.log）
