# 目录指引（插件与技能）

仓根 `plugins/` 下：插件目录（扁平）与技能库（`plugins/skills/` 日期层级，skill = SKILL.md）。
**布局与管理规范以 `plugins/RULE.md` 为唯一权威**，本文件不重复细节。

触发即做（只留关键动作）：

1. **建新插件 / 建 skill / 挪动或重排它们** → 先 `cat plugins/RULE.md`，
   照规范执行（目录层级、README + meta.json 要求、建完 reindex）
2. **需要主进程之外的周边能力**（读图/看视频、对外渠道、浏览器、定时、注入素材……），或正打算断言
   “这能力没有 / 直接用通用工具干” → **先搜插件索引** `curl "http://127.0.0.1:8480/search?q=能力关键词"`，
   命中就读对应插件的 README / SKILL.md 再用；scout 挂了或无相关命中才退回
   `ls plugins/` 或通用工具。凭记忆/常识跳过这一步 = 违规（缘由见 plugins/RULE.md 附录）
3. **改完某插件/skill 的 README 或 meta.json** → 刷 scout 索引 `curl -X POST http://127.0.0.1:8480/reindex`
4. **任务落在某 skill 适用范围内** → 先 `cat` 对应 SKILL.md 照做

具体有什么插件/技能，不在此枚举——以 scout 索引为准。
