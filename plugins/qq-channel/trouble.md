# qq-channel — 排障

1. **"响应无 access_token"**：先对三个时间——源码 mtime、二进制 mtime、日志里 bug 出现的时间；二进制比源码旧 = 没重编（09-20 案例）。二进制是新的仍报错 = 看 QQ 原始响应体（别信错误信息，09-19 案例实际是 `expires_in` 类型不符）。详见 `LESSONS.md`。
2. **数据写到旧路径**（挪目录后）：`data_dir` 在启动时算定，存活进程仍写旧绝对路径 → 杀进程、迁数据、新路径重拉；`readlink /proc/<pid>/cwd` 确认进程实际位置。
3. **双份转发 / 重复入队**（让位没生效才会见）：日志里搜「让位退出」——没出现 = 旧桥卡在阻塞调用里超过 2s，手动 `kill` 它；`cat data/channels/qq/bridge.gen` 看现在谁持有。
4. **消息被 QQ 拒**：msg_seq 回退——确认 `data/channels/qq/msg_seq` 存在且比上次发送值大。
5. **QQ 发了没反应**：`journalctl --user -u qq-bridge.service -n 80` 看有没有"已订阅 /events"（每个 bot×实例一条）；再手动 `curl -X POST {实例}/enqueue` 验证实例可达。
6. **临时测试起一个桥之后**：它接管了租约，服务那个退 0 且不会被自动拉起 → 测完 `systemctl --user restart qq-bridge.service` 交回。
