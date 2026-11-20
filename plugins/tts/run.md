## 启动 / 停止 / 用

```bash
./start.sh                       # 起常驻 HTTP 服务 :8092（模型后台加载，就绪前 /health 返回 ok:false）
./say.sh "要念的文本" [输出.wav]  # 直接合成；服务没起会自动拉起；stdout 打印 wav 路径
./stop.sh
```
