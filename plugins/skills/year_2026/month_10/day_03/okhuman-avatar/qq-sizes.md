## 3. 裁 QQ 头像尺寸梯度

```bash
python3 -c "
from PIL import Image
im=Image.open('references/xiaoK-8451_s33.png').convert('RGB')
for s in (40,64,128,256,512):
    im.resize((s,s), Image.LANCZOS).save(f'/tmp/xk8451_{s}.png')
"
```
