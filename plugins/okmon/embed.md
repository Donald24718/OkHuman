# okmon — 实例界面入口（iframe 内嵌各实例自己的 webui）

webui 顶部一排按钮，自动列出全部运行中的 okhuman 实例，点开就在同页 iframe 里加载**该实例自己的 webui**——不是复制品，功能 100% 等价，OkHuman 前端更新这里自动跟着更新。选择记在 localStorage，实例掉了自动切到别的。

## 边界（诚实列）

- **一次只看一个实例**（切 iframe 的 src）。**没有**跨实例统一视图/统一搜索/群发——那要在 okmon 侧做代理聚合：浏览器跨端口 `fetch` 会被 CORS 挡，因为 **OkHuman 实例不发 `Access-Control-Allow-Origin`**（实测）。想做就走 okmon 后端代理，别在前端硬连。
- 内嵌能成立的前提是实例**不发** `X-Frame-Options` / `CSP frame-ancestors`（实测没有），且实例 UI 没有 framebusting JS（实测没有）。**OkHuman 哪天加上这些头，这里就失效。**
- 只有**有监听端口**的实例才出现在按钮里（纯 CLI 的没有界面可嵌）。
- 切换实例会重载该页（71KB HTML + 40KB marked.min.js，本机无感）。
- 2 秒自动刷新**不会**重载 iframe（`dataset.port` 守卫）——否则界面每 2 秒闪一次，没法用。
