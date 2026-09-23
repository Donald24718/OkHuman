# 目录指引（插件与技能）

`/media/xhy/512nv固态/OkHuman_p/` 下：插件目录（`Frequently Used Plugins/` 扁平、
`Rarely Used Plugins/` 日期层级）与技能库（`OkHuman_skill/` 日期层级，skill = SKILL.md）。
**布局与管理规范以 `OkHuman_p/RULE.md` 为唯一权威**，本文件不重复细节。

触发即做（只留关键动作）：

1. **建新插件 / 建 skill / 挪动或重排它们** → 先 `cat /media/xhy/512nv固态/OkHuman_p/RULE.md`，
   照规范执行（目录层级、README + meta.json 要求、建完 reindex）
2. **需要主进程之外的周边能力**（规则 8：对外渠道/浏览器/定时/注入素材等）→ 先 scout 搜索
   `curl "http://127.0.0.1:8480/search?q=关键词"`，命中读对应 README / SKILL.md 再动手
3. **改完某插件/skill 的 README 或 meta.json** → 刷 scout 索引（命令见 RULE.md 附录）
4. **任务落在某 skill 适用范围内** → 先 `cat` 对应 SKILL.md 照做

具体有什么插件/技能，不在此枚举——以 scout 索引为准。
