## 5. 排查与清理（心跳自查用）

```bash
# 找 PPID=1 的脱管 bash（我起的等待循环都长这样）
ps -eo pid,ppid,etime,cmd | awk '$2==1' | grep -E "^.*bash /tmp/okhuman-bash|while pgrep|while ! grep"
# etime 异常大（小时/天级）即可疑；确认它是不是自匹配：
tr '\0' ' ' < /proc/<pid>/cmdline; echo
cat /tmp/okhuman-bash-*.sh        # 看它在等什么，那个东西还在不在
kill <pid>                        # 确认是我自己起的等待循环才杀
```
杀前必查归属：**别碰** `./okhuman`（我自己）、`serve.server --port 8080`（我依赖的 LLM）、
`./scout`、`./okmon`、`./okhuman-cron`、插件二进制。只杀 `/tmp/okhuman-bash-*.sh` 这类我自己的一次性循环。

## 6. 待验证（不是结论，下次遇到再试）

- 10-17 那次 cut 失败的具体原因：`ffmpeg` 一次喂 **85 个 PNG 输入 + `hstack=85`** → `exit 8`。
  推测分批（8~12 张一批）能过，**未实测**，别当已验证路线用。

## 坑

- **`pgrep -f <模式>` 会匹配到发起它的 `bash -c '<整条命令>'` 自身**（模式串就在自己 argv 里）
  → 链式等待（while pgrep; do sleep; done）死循环。用精确进程名（`pgrep -x`）或
  `pgrep -f` 后 `grep -v $$` / 匹配可执行文件全路径。
