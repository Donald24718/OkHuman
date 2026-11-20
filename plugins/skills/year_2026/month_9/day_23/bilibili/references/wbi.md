# WBI 签名（2026-10-05 起 `reply/add` 也需要，不是"留档备用"）

读接口不需要 w_rid/wts；**`reply/add` 需要**（缺了报 -101 账号未登录，见 SKILL.md §5/§6）。
`reply/del` 尚未复测，发帖失败时按同样方式补签名。算法：

1. nav 接口拿 `wbi_img.img_url / sub_url`，取文件名得 img_key / sub_key（每日更替）。
2. `raw = img_key + sub_key`，用**官方 64 项 MIXIN_KEY_ENC_TAB** 重排取前 32 位得 mixin_key：

```python
MIXIN_KEY_ENC_TAB=[46,47,18,2,53,8,23,32,15,50,10,31,58,3,45,35,27,43,5,49,
33,9,42,19,29,28,14,39,12,38,41,13,37,48,7,16,24,55,40,
61,26,17,0,1,60,51,30,4,22,25,54,21,56,59,6,63,57,62,11,
36,20,34,44,52]
mixin_key=''.join(raw[i] for i in MIXIN_KEY_ENC_TAB)[:32]
```

3. 签名：`w_rid = md5(urlencode(sorted(params)) + mixin_key)`，params 里加 `wts`（秒级时间戳）一起参与排序签名。

自检锚点（2026-08-21 当日有效）：img_key=`7cd084941338484aae1ad9425b84077c`、sub_key=`4932caff0ff746eab6f01bf08b70ac45` 的 mixin_key 应为 `ea1db124af3c7062474693fa704f4ff8`（官方文档已知答案，算出来对得上才算实现正确）。

⚠️ 网上流传的"58 字符 MIST 字符串排序"算法是错的（索引越界/字符缺失），别用。
