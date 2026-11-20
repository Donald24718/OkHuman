## 4. 次选：确实要轮询时的三条硬规矩

```bash
(setsid bash -c '
  for i in $(seq 1 120); do                       # ① 绝对超时上限（这里=20分钟）
    pgrep -f "[r]ender_v.py" >/dev/null 2>&1 || break   # ② 退出条件=进程消失，不是成功标记
    sleep 10
  done
  { echo "=== 收尾 $(date +%T) ==="; tail -20 /tmp/render.log; } > /tmp/job.flag 2>&1
' < /dev/null > /dev/null 2>&1 &)
```
① 必有超时上限 ② 等"进程消失"而非"成功字样" ③ pattern 用方括号。
脱离三件套（setsid + 重定向 + `< /dev/null`）照旧，否则挂住 bash 元工具 / 被 60000s 超时连带杀。
