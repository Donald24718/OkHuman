## 构建与测试

```bash
CGO_ENABLED=0 GOPROXY=off /usr/local/go/bin/go vet ./...
CGO_ENABLED=0 GOPROXY=off /usr/local/go/bin/go test -count=1 ./...   # 7 个测试：字段容错/坏meta可见/同名不吞/内联折叠/权重/kind/分词
```
纯标准库、无依赖、CGO 关。**不做交叉编译**（只 Linux）。

## 运维

- 本目录是**独立 git 仓库**（每个插件各自一个，按主人的规矩）。回滚点：`6f5afe6` = 拆向量前的原状。
- 旧二进制备份：`scout.bak-2026-11-05-pre-no-vector`；旧索引快照（含向量）：`index.json.bak-2026-11-05-pre-no-persist`。
- 起法：本机由 user 单元 `okhuman-scout` 管（已 enable，开机自起；`systemctl --user start|stop okhuman-scout`）。手工起：`(setsid plugins/scout/scout </dev/null >>/tmp/scout-logs/scout.log 2>&1 &)`。
- embed 后端（`:8591`）已不再需要，随本次改造停止。
