## 3. 首选方案：把收尾和任务绑在同一个脱离进程里（实测，推荐）

不要在外面轮询。让任务自己（无论成败）落一个 flag，事后任何时候 `cat` flag 取结果：

```bash
(setsid bash -c '( 长任务... ) > /tmp/job.log 2>&1; echo "exit=$?" > /tmp/job.flag' \
   < /dev/null > /dev/null 2>&1 &)
```
实测：任务 `exit 3` 失败，`/tmp/job.flag` 仍写出 `exit=3`，且**无残留进程**。
要点：`echo "exit=$?"` 在括号外 → 成败都写；flag 里带退出码，失败一眼可见。
取结果：`cat /tmp/job.flag; tail /tmp/job.log`。
