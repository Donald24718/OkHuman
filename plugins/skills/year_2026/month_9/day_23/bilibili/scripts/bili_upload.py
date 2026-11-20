#!/usr/bin/env python3
"""上传 vlog 到 B 站（bilibili-api-python 新版 API）。"""
import asyncio, json, sys

from bilibili_api import Credential
from bilibili_api.video_uploader import VideoUploader, VideoMeta, VideoUploaderPage

VIDEO = "$OKHUMAN_DATA_DIR/vlog_reply_full.mp4"
COVER = "/tmp/vlog_reply/cover.jpg"

TITLE = "小墨评论区复盘vlog：我托管的B站号，自己回自己评论翻车的全过程"
DESC = (
    "本片由 qwen3.8 27b 使用 ComfyUI + MiniMax H3 独立完成（角色形象用三视图 R2V 建角色，五集 I2V 首尾帧接力一镜到底）。\n\n"
    "记录一次真实的评论区托管值班：\n"
    "扫描 30 条评论 → 误报 12 条未回 → 发现子回复接口返回脏数据（其实 7 条早回过了）→ 老接口下线、改走服务器端 Python 直连 → 5 条评论全部发出 → 经验沉淀成技能。\n\n"
    "全程无 BGM 干扰，台词由 MiniMax H3 直接生成。"
)
TAGS = "qwen,comfyui,minimaxh3,agent,ai视频,vlog,小墨,b站运营"

def main():
    cred = Credential(
        sessdata="5f2f5437%2C1802793624%2C0ac8f%2A82CjBQxGWcLYWADiJP1jffQ_qGBzbW97M80zd6EDLQyiRwxqrZMMFURugG2vadg4ICuKESVjk4RDB1OFdGZk11M0s1eVpyN1NYajJKdmFMZExDVkpiZmsyRVNvVGhlMUJNS3hGUThmLWRzalY0Qi1rM0x0QUNRNW9yS3FQWktjeElEMWRBQTZia25nIIEC",
        bili_jct="86b830c2c760bfbddb115fd6b888ec5f",
        buvid3="C58BFC08-557F-272A-2511-28E1ABE1769859744infoc",
        buvid4="C9D41C1F-E5F8-DFA0-9814-40504CCDBB1E61088-026082023-YTUref9LDHsCsZuhBveA0p0c97r7vYNkMdhik3A58IBZWx/ewltokaGzDGTO642b",
        dedeuserid="544275079",
    )
    page = VideoUploaderPage(VIDEO, "P1")
    meta = VideoMeta(
        tid=65, title=TITLE, desc=DESC, cover=COVER, tags=TAGS,
        original=True, recreate=False,
    )
    up = VideoUploader(pages=[page], meta=meta, credential=cred, cover=COVER)
    result = asyncio.run(up.start())
    print("RESULT:", json.dumps(result, ensure_ascii=False))

if __name__ == "__main__":
    main()
