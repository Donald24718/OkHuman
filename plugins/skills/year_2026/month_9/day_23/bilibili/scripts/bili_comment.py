#!/usr/bin/env python3
"""B站评论操作参考脚本（2026-08-21 本机实测）。

用法：
  # cookie 文件：一行 `name=value; name2=value2`，必须含 SESSDATA + bili_jct，chmod 600
  python3 bili_comment.py --cookie-file /tmp/bili_cookie.txt scan  --aid 117125972760499
  python3 bili_comment.py --cookie-file /tmp/bili_cookie.txt aid   --bvid BV1Zr8F6uEHh
  python3 bili_comment.py --cookie-file /tmp/bili_cookie.txt post  --aid <AID> [--root <rpid>] [--parent <rpid>] --text "内容"
  python3 bili_comment.py --cookie-file /tmp/bili_cookie.txt del   --aid <AID> --rpid <rpid>

要点：
  - 发/删走 /x/v2/reply/add 与 /x/v2/reply/del。⚠️ 2026-10-05 起 reply/add 必须带 WBI 签名（wts+w_rid，见 SKILL.md §5 与 references/wbi.md）；本脚本未实现签名，直接照抄会报 -101。
  - csrf 用 cookie 里的 bili_jct，body 和 header 各一份。
  - 成功判定以响应 code==0 为准；读接口有写后缓存滞后，别拿它当验证。
  - post 不带 --root 即发全新根评论（root=parent=0，2026-08-21 实测）；
    回复某评论才传 --root <rpid>（--parent 缺省等于 root）。
    注意：根评论不能把 root 传成 aid，会报 12006。
"""
import argparse, json, sys, time, requests

HDRS_TMPL = {
    "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/120.0.0.0 Safari/537.36",
    "Referer": "https://www.bilibili.com/",
    "Origin": "https://www.bilibili.com",
    "x-csrf-token": "",
    "Cookie": "",
}

def load_cookie(path):
    raw = open(path).read().strip()
    hdrs = dict(HDRS_TMPL)
    hdrs["Cookie"] = raw
    jct = None
    for kv in raw.split(";"):
        kv = kv.strip()
        if kv.startswith("bili_jct="):
            jct = kv[len("bili_jct="):]
    if not jct:
        sys.exit("cookie 文件里没有 bili_jct")
    hdrs["x-csrf-token"] = jct
    return hdrs, jct

def check_login(hdrs):
    r = requests.get("https://api.bilibili.com/x/web-interface/nav", headers=hdrs, timeout=30)
    j = r.json()
    if j.get("code") != 0 or not (j.get("data") or {}).get("isLogin"):
        sys.exit(f"登录态失效: {json.dumps(j, ensure_ascii=False)[:200]}")
    up_mid = j["data"]["mid"]
    return str(up_mid)

def cmd_scan(hdrs, args):
    """读主列表：返回每个 top 的 rpid/作者/文本 + 我的回复情况（可信读法）。"""
    up_mid = args.up_mid or check_login(hdrs)
    tops = []
    for pn in range(1, 5):
        r = requests.get("https://api.bilibili.com/x/v2/reply", headers=hdrs,
            params={"type": 1, "oid": args.aid, "pn": pn, "ps": 20, "sort": 1}, timeout=30)
        j = r.json()
        if j.get("code") != 0:
            sys.exit(f"读失败: {json.dumps(j, ensure_ascii=False)[:200]}")
        rs = (j.get("data") or {}).get("replies") or []
        tops.extend(rs)
        if len(rs) < 20:
            break
    out = []
    for t in tops:
        kids = t.get("replies") or []
        out.append({
            "rpid": t["rpid"], "name": (t.get("member") or {}).get("uname"),
            "text": ((t.get("content") or {}).get("message") or "")[:60],
            "rcount": t.get("rcount", 0),
            "i_replied": (str(t.get("mid")) == up_mid) or any(str(k.get("mid")) == up_mid for k in kids),
            "kids": [{"rpid": k["rpid"], "name": (k.get("member") or {}).get("uname"),
                      "mid": str(k.get("mid")), "parent": k.get("parent"),
                      "text": ((k.get("content") or {}).get("message") or "")[:40]} for k in kids],
        })
    print(json.dumps({"up_mid": up_mid, "n": len(out), "tops": out}, ensure_ascii=False, indent=1))

def cmd_aid(hdrs, args):
    r = requests.get("https://api.bilibili.com/x/web-interface/view",
                     headers=hdrs, params={"bvid": args.bvid}, timeout=30)
    j = r.json()
    if j.get("code") != 0:
        sys.exit(f"查 bvid 失败: {json.dumps(j, ensure_ascii=False)[:200]}")
    print(j["data"]["aid"])

def cmd_post(hdrs, args):
    # 不带 --root：发全新根评论 root=parent=0；带 --root：回复该评论
    root = "0" if args.root is None else str(args.root)
    parent = root if args.parent is None else str(args.parent)
    data = {"type": "1", "oid": str(args.aid), "root": root,
            "parent": parent, "message": args.text, "plat": "1",
            "csrf": args.jct}
    h = dict(hdrs); h["Content-Type"] = "application/x-www-form-urlencoded"
    r = requests.post("https://api.bilibili.com/x/v2/reply/add", headers=h, data=data, timeout=30)
    j = r.json()
    print(json.dumps({"code": j.get("code"), "msg": j.get("message"),
                      "rpid": (j.get("data") or {}).get("rpid"),
                      "toast": (j.get("data") or {}).get("success_toast")}, ensure_ascii=False))
    if j.get("code") != 0:
        sys.exit(1)

def cmd_del(hdrs, args):
    data = {"type": "1", "oid": str(args.aid), "rpid": str(args.rpid), "csrf": args.jct}
    h = dict(hdrs); h["Content-Type"] = "application/x-www-form-urlencoded"
    r = requests.post("https://api.bilibili.com/x/v2/reply/del", headers=h, data=data, timeout=30)
    j = r.json()
    print(json.dumps({"code": j.get("code"), "msg": j.get("message")}, ensure_ascii=False))
    if j.get("code") != 0:
        sys.exit(1)

def main():
    ap = argparse.ArgumentParser(description="B站评论操作（服务器端直连）")
    ap.add_argument("--cookie-file", required=True)
    sub = ap.add_subparsers(dest="cmd", required=True)

    s = sub.add_parser("scan");  s.add_argument("--aid", required=True); s.add_argument("--up-mid", default=None)
    a = sub.add_parser("aid");   a.add_argument("--bvid", required=True)
    p = sub.add_parser("post");  p.add_argument("--aid", required=True); p.add_argument("--root", default=None)
    p.add_argument("--parent", default=None); p.add_argument("--text", required=True)
    d = sub.add_parser("del");   d.add_argument("--aid", required=True); d.add_argument("--rpid", required=True)

    args = ap.parse_args()
    hdrs, jct = load_cookie(args.cookie_file)
    args.jct = jct
    {"scan": cmd_scan, "aid": cmd_aid, "post": cmd_post, "del": cmd_del}[args.cmd](hdrs, args)

if __name__ == "__main__":
    main()
