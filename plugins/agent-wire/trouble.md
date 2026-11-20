# agent-wire — 排障

- `认不出我自己：没有 OKHUMAN_SELF_URL。…` —— 没声明身份时 `send` 会**前置拒绝**（不派一个回信永远投不回的活）。在实例里跑由主程序注入；终端里手跑自己声明：`OKHUMAN_SELF_URL=http://127.0.0.1:<端口> agent-wire send …`。config 里写 `self` 已经**无效**（2026-11-12 删掉这条兜底）。`list`/`pending` 不需要身份，任何时候可跑。
- `拒：X 正在 run` —— 正常：规则如此。要等就自己隔几秒再试一次（`list` 可先看状态）。
- 拿不到正文但对方确实跑了：多半是对方只发 `delta` 没有 `text`——本插件按 `delta.text` 累加，若主程序改了事件形态，先看 `curl -H "Authorization: Bearer <token>" "http://<桥>/events/poll?since=0"` 里 `frames` 的真实形态（一帧是整块 SSE，要按 `data:` 行解析）。
- 跨机不可达：先确认对端的 lan-bridge 在听（`ss -ltn | grep 860`）与 token 正确（peer 的 token/inst 怎么写见 `config.md`）。
- 接上的 watcher「一秒就没了」、记录还留着而日志不见：多半是等待记录被**非原子**重写（先清空再写），刚起来的 watcher 读到空文件、`Unmarshal` 失败就静默退——写记录一律临时文件 + `rename`（2026-11-10 修）。判断进程活死看 `/proc/<pid>`，别用 `pgrep -f`（本场景数不准）。
