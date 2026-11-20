# okmon — 排障

- **端口冲突**：okmon 启动日志（`/tmp/okmon.log`）会报；换 config.json 的 port。
- **启动报"12 秒内没起来"**：看它附带的 `/tmp/okmon-<name>.log` 尾 3 行；常见是该服务本身不是常驻（打印用法就退出）→ 改成 `cli: true`。
- **启动按钮点了没反应 / 报"12 秒内没起来"且日志空**：workdir 含空格（插件在 `Frequently Used Plugins/` 下）——启动脚本必须对 workdir 与日志路径单引号包裹（`shellQuote`）。2026-10-04 修：未引号时 `cd <带空格路径> && ...` 报 `cd: 参数太多`，`&&` 短路 → 服务根本没拉起，且报错落在 okmon 自己的 stderr 而非 `/tmp/okmon-<name>.log`，所以日志看着是空的。09-21 插件目录重排后 qq-channel/browser/cron/scout 的启动按钮其实一直失效，只是此前都是手工启动没暴露。
- **stop 误杀**：定位端口优先（okhuman 实例只按端口找），不会误伤 cmdline 里含 "okhuman" 的路径进程。
- **nvidia-smi 缺失/失败**：显卡面板为空，进程面板不受影响。
- **改了 webui.html 页面没变**：`webui.html` 是 `//go:embed` 烘进二进制的，**不是**运行时读盘；只重启进程不够（本次就踩了：改完 HTML 重启后浏览器里 `#insts` 根本不存在）。正确顺序：改 → `go build -o okmon .` → 重启 → 浏览器实测。
