# browser — 自测我自己写的前端（count / errors / eval）

给"我自己写完前端，自己开浏览器验"这条链用（2026-10-07 加）。

```bash
browserctl <port> count  "#stage .l"        # 数元素个数
browserctl <port> errors [--clear] [--all]  # 页面 JS 异常 / console 报错
browserctl <port> eval   "<js>"             # 任意求值（可模拟输入并提交）
```

`errors` 由 **daemon 侧**持续记录（`/tmp/okhuman-browser-<port>.errors`，JSONL）：browserctl 是短连接，命令之间没人接事件，异常只能由常驻的 browserd 收。**有报错 → 退出码 1**，测试脚本可以直接 `&&`/`if` 判，不用肉眼比对输出。

一条完整的自检（实测跑通）：

```bash
browserctl 8732 open http://127.0.0.1:8460/
browserctl 8732 eval '(()=>{const i=document.getElementById("in");
  i.value="来杯麦酒";document.getElementById("f").requestSubmit();})()'
sleep 6
browserctl 8732 count "#stage .l"   # 行数变多 = 玩家那句话真落地了
browserctl 8732 errors              # 干净页退出码 0；有 JS 异常退出码 1
```

实测：干净页 `errors` 退出码 0；故意放一个 `undefined_fn()` 的页面退出码 1，并把 `Uncaught ReferenceError: undefined_fn is not defined @file:1:39` 连行号打出来。

## 失败与限制（都是实测踩出来的）

- 同一个异常 chromium 会重放（重新 enable、切 tab 都会再来一遍）→ **写入侧按 kind+text 去重**。
- 资源 404（favicon 之类）默认不计入，否则真异常被埋 → 要看全部用 `--all`。
- `console.error` 只有在 daemon 挂上之后**新产生**的才收得到——页面加载瞬间打的那条可能漏（**异常不会漏**，Runtime.enable 会补发）。
