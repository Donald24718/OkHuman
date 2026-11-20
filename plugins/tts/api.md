## 接口（零依赖，只用标准库）

```bash
curl -s -X POST http://127.0.0.1:8092/tts \
  -H 'Content-Type: application/json' \
  -d '{"text":"你好，English too","voice":"zf_xiaoyi","format":"json"}'
# -> {"path":"/tmp/tts_http_out/tts_xxxx.wav","seconds":1.23,"ms":456}

curl -s "http://127.0.0.1:8092/tts?text=你好&format=wav" -o out.wav   # 直接拿音频字节
curl -s http://127.0.0.1:8092/health                                   # {"ok":true,...}
```

合成加锁串行（Kokoro 非线程安全），并发请求排队即可。
