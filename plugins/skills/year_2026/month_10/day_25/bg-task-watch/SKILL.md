# 后台任务与等待循环（bg-task-watch）— 母文档只做路由

我怎么起长任务、怎么等它结束。错法会让等待循环永挂、占住进程组。

## 要做什么 → 读哪份

| 要做什么 | 文件 |
|---|---|
| 为什么不能裸用等待循环、两个根因 | `incident-and-causes.md` |
| 首选做法：把收尾和任务绑在同一个脱离进程里 | `bind-together.md` |
| 确实要轮询时的硬规矩 | `polling-rules.md` |
| 找出脱管的等待进程、清理、以及其它坑 | `find-and-clean.md` |

## 遇到什么现象 → 读哪份

| 现象 | 文件 |
|---|---|
| 等待循环不退出、命令挂到超时 | `incident-and-causes.md`、`find-and-clean.md` |
| 任务已经失败退出，我却还在等 | `incident-and-causes.md` |
| 按名字查进程查到一堆、分不清是不是自己 | `find-and-clean.md` |
| 起的长驻服务被连带杀掉 | `find-and-clean.md`
