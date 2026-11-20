## 2. 远程执行的硬规矩

- **pkill -f 的写法常打不到远端进程**（实测：两条已'杀掉'的渲染线继续跑了 40 分钟，把单段耗时从 400s 拖到 1300s）。可靠做法：先 `ssh ey pgrep -af 名字` 拿 PID，再 `ssh ey kill -9 <PID>`，然后再 pgrep 复核为 0 才算停掉。

- **杀掉本地 ssh 不会停远端的活**：`timeout N ssh ey '长任务'` 超时或本地 pkill 后，远端进程继续跑并占 GPU（实测：三条已'杀掉'的渲染线在对面又烧了二十多分钟，还和新线重复）。停法：远端把任务写在脚本里，另开一条 `ssh ey 'pkill -f 脚本名'`；判断是否在跑要 `ssh ey pgrep`，不能只看本地。

- **提权必须显式**：`echo <密码> | sudo -S -k bash -c '…'`，或 `SUDO_ASKPASS=<echo 密码的脚本> sudo -A -k`。没提权的 `nmcli`/`apt` 报 `Insufficient privileges`，或**静默什么都不做**。
- **`setsid nohup sudo … &` 会静默死掉**（日志空、未执行）。要脱离会话就先 `ssh host 'cat > /tmp/x.sh'` 写脚本，再同步 `sudo -S -k bash /tmp/x.sh`。
- **多行/带引号的命令走脚本文件**：`ssh host 'cat > /tmp/x.sh' <<'EOF' … EOF`，然后 `printf pw | ssh host 'sudo -S -k bash /tmp/x.sh'`。在命令行里嵌引号必踩 `invalid quotes`、`$(…)` 被本地展开、`sudo -S` 吃掉脚本首行。
- **判定只能在控制侧做**：远端自己说成功不算（桥没建成时它的 `ping` 照样通，因为旧路由还在）。判据是"我从这边能不能到目标"。跨机比对文件时每台的文件名要唯一——把两份同名文件都 `cat > $(basename …)` 会互相覆盖，据此做的 grep 计数全是假的（11-11 我据此误判 .3 的 README 缺两条规矩，实际它有三处旧说明）。 **别拿二进制字节数判源码异同**：编译时绝对路径会编进映像——实测同一份 `agent-wire.go` 两台在相同目录编译出的映像 sha 完全一致，目录名不同就差 8 字节（2026-11-12）；比源码就 `sha256sum` 源文件 + git，别比产物。

## 9. 隐私

IP、主机名、账号、密码一律不外泄（规则见 credentials 与 prompts/03-channel-safety）。
