# agent-wire — 配置与构建

`config.json` 与可执行文件同目录；**不再写 `self`**。

```json
{"peers": [
   {"name":"小E","url":"http://127.0.0.1:8451"},
   {"name":"小C","url":"http://127.0.0.1:8452"},
   {"name":"小A","url":"http://192.168.1.2:8602","token":"<对方 lan-bridge token>","inst":"8451"}]}
```

- 本机实例直连 `127.0.0.1:<port>`；**跨机走对方的 lan-bridge**（带 `token`，用 `Authorization: Bearer`），**桥挂多实例时 peer 必须写 `"inst":"<上游 id>"`**（缺了桥直接 400，不再静默落默认实例）；桥的白名单需要 `/status`、`/events/poll`、`/enqueue`（默认已放开）。
- **「我自己」由跑我的实例声明**：主程序起工具子进程时注入 `OKHUMAN_SELF_URL` 与 `OKHUMAN_DATA_DIR`（`internal/tools.SetSelfID`，2026-11-11），回信目标与等待目录都跟着它走。各实例的 config 里已删掉 `self`（2026-11-11，主人定）：字段仍被代码认，只作手跑兜底，**2026-11-12 已删掉该兜底**（同机两实例共用一份配置会认错人）。起因：8452 直接用了 8451 的这份 config，回信投进 8451、两个实例的等待记录落在同一目录互相搅。`list` 从此不标「(我自己)」——agent 自己知道哪个是它。

## 构建

```bash
cd "<本目录>" && go build -o agent-wire agent-wire.go   # 只用标准库
```
