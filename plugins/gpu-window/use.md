# gpu-window — 用法与 pid 来源

## 唯一的 pid 来源：显存归属

`map` 列出每张卡上「哪个 pid 占多少、对应 config 里哪个服务」。**不要拿 unit 的 MainPID 去切**：多进程服务（本机 8080 = python 主进程 + `build-ab/strata` + `strata-vision`）打在 MainPID 上会**"成功但无效"**——状态变成 CHECKPOINTED 而显存不动。

## 命令

```bash
/usr/local/go/bin/go build -o gpu-window .     # 只依赖 libcuda，不需要 CUDA 头文件
./gpu-window map                                # 每张卡的显存归属 + 哪个是登记的服务（pid 从这里取）
./gpu-window status <服务名|pid>
./gpu-window yield  <服务名|pid> [--force]      # 换出到内存 = 让卡
./gpu-window resume <服务名|pid>                # 换回来
./gpu-window window <服务名> --for 120 | --cmd '…出图命令…'   # 让卡 → 做事 → 无论成败都换回
```

退出码：`2`=闸门拦下（有实例正在 run）｜`3`=该服务标了 protected｜`1`=其它错误。

## 测试

`go vet ./...`、`go test ./...`（5 个，不依赖 GPU）：`match` 的字符串/数组两种写法、两段 nvidia-smi CSV 解析、**只挑出持有显存的 pid**（MainPID 陷阱）、闸门的 `running` 判定、state 枚举映射。
