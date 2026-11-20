## 3. 改它网络之前

先确认还有**第二条控制通道**（sd-box 的 WiFi `192.168.0.103` 同样可 ssh）。没有第二条路就不要动它的网络，否则只能等人到现场。

## 4. sd-box 上 NetworkManager / netplan 的实测结论

- `/etc/netplan/90-NM-*.yaml` 是 NM 反写的镜像：改 NM 就是改配置源。存在 `connection.permissions: user:sd`、`autoconnect: false` 的连接，NM 不会自动接管它们。
- **建桥必须显式激活端口**：`nmcli con up br0` 只会得到 `master waiting for slaves`、`bridge link | grep -c forwarding` = 0。补 `nmcli con up br0-p1`、`nmcli con up br0-p2` 才真的挂上——**判据：forwarding 计数 = 2**。
- `bridge-slave` 连接不接受 `ipv4.*`（`无效或不允许的设置 "ipv4"`）；端口本来就不该带 IP。
- **同一个 /24 挂在两个口上 → 出接口选择不可控**，表现就是"连不上"。临时 `ip route add X/32 dev …` 会被 NM 重连冲掉；持久写法是 `nmcli con mod <连接名> ipv4.routes "X/32"`（会写进 netplan 镜像）。
- 激活桥时 NM 会把桥的 IP 反写进端口的原连接 → 两个口同一个 IP、ARP 打架（症状：它连上游都 ping 不通）。所以端口连接保持无 IP，旧连接别留着同一份地址。

## 5. 当前生效配置（2026-11-01；**未经重启验证**）

`br0` 持 `192.168.1.1/24`；`enp3s0`（接本机）+ `enp2s0`（接另一台）为无 IP 端口，都在 forwarding；原连接 `<用户>`(=.1)、`自由架构`(=.4) 保留但 `autoconnect no`。备份：`/root/net-backup-20261101/`（netplan.tgz + 5 个连接配置）。退回原状：删 `br0-p1/br0-p2/br0`，把那两条 `connection.autoconnect yes` 并 `nmcli con up`。
**未验证：开机后桥能否自动起**（autoconnect 都是 yes，但没重启证明过）。重启 sd-box 后第一件事跑 `bridge link | grep -c forwarding`，=2 才算成。

## 6. 装包

装之前先确认真需要：对"手动地址"的机器，DHCP 服务永远不会被请求（dnsmasq 装了即无用，已 purge）。卸载时 **`dnsmasq-base` 要保留**，NetworkManager 的共享模式用它。
