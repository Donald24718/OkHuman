# asr — 起停、配置、两机实况、与 voice-chat 的关系

## 启动 / 停止

```bash
./start.sh        # 起 llama-server :8090，探活到 HTTP 200 才返回
./stop.sh         # 停（pkill 模式限定 Qwen3-ASR，不误伤别的 llama-server）
```

## 配置（config.json）

| 键 | 默认 | 说明 |
|---|---|---|
| `port` | 8090 | 服务端口 |
| `model` / `mmproj` | models/ 下两个 gguf | 相对插件目录 |
| `threads` | 16 | `-t` |
| `ctx` | 8192 | `-c` |
| `model_name` | qwen3-asr-0.6b | 请求里 `model` 字段的取值 |

## 两台机器上的实况（2026-11-06 实测，两台都能本机跑，不必跨机）

| | 生产 本机（.2） | 另一台（.3） |
|---|---|---|
| 二进制与模型 | 本插件目录内 `bin/llama-server` + `models/`（同结构） | 同结构（换根见 `host-crash-autostart/services.md`），`models/Qwen3-ASR-0.6B-bf16.gguf`(1.5 G)+`mmproj-…`(378 M) |
| 端口 | 8090 | 8090（各绑本机回环，互不冲突），已实测 `:8090`=200 |
| 常驻方式 | **不是 systemd 单元**（脱管进程），重启不自启 → 用 `./start.sh` | 同样不是单元 |
| 查它在不在 | `lan-ssh/remctl.sh matrix` / `owns 8090` | 同 |

## 与 voice-chat 的关系

voice-chat 不再自带 ASR：它的 `start.sh` 探活 :8090，不通就调用本插件 `start.sh`。本插件挂了 voice-chat 的识别会报错重试；voice-chat 挂了不影响本插件。
