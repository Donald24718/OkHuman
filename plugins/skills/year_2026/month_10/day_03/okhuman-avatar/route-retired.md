## 2. 生成路线（**已退役**：所用模型 2026-10-06 已删）

> ⚠️ 该模型已于 2026-10-06 按主人判定删除（照下面的连线提交会报模型缺失）；`references/` 里已生成的立绘资产仍有效。要重新生成走 H3 REF2I，或按 comfyui skill 的退役说明重下。

**完整节点图（含连线）与 prompt、negative 都嵌在 PNG 的 `prompt` 元数据里**（8 个节点：
UNETLoader `qwen_image_2.1_int8_convrot` + CLIPLoader `qwen3vl_8b_int8_convrot`(type=qwen_image) +
VAELoader `qwen_image_2.1_vae_bf16` + TextEncodeQwenImage21(`resolution=1024`) +
KSampler(`euler`/`simple`/`cfg 1.0`/`steps 25`) + EmptyLatentImage 1024×1024）。
**实测只有 `prompt` 一个字段，没有 `workflow` 字段**——要连线就从这个 JSON 里读，别凭记忆重连。

```bash
python3 -c "
from PIL import Image; import json
d=json.loads(Image.open('references/xiaoK-8451_s33.png').info['prompt'])
for nid,n in d.items():
    if isinstance(n,dict) and n.get('class_type')=='TextEncodeQwenImage21':
        print(n['inputs']['prompt']); print('---NEG---'); print(n['inputs']['negative_prompt'])
    # 要整张节点图：print(nid, n.get('class_type'), n.get('inputs'))
"
```

要改端口/配色重跑：把 prompt 里的端口数字、发光色（red/blue/violet）、发梢语义三处改掉即可，
其余保持家族统一。
