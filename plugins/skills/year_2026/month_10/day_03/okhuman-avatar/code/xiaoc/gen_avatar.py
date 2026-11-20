#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""
小G (orca, :8454) 代码头像生成器
规范: $OKHUMAN_DATA_DIR/avatar-jobs/SPEC.md v1 (2026-11-07 小A定)
身份: 端口 8454 | 主色 #BFE9F5 冰蓝白(主体黑白,发光主色) | 徽记=虎鲸剪影(背鳍+眼后白斑)
硬性: 仅标准库+Pillow10.2; 无模型/联网/贴图; 无未固定随机 → 两次运行字节一致
小A补充(2026-11-07):
  ① 数字自适应缩字号, 总宽<=开环内径*0.78 (不贴环)
  ② 徽记与环留>=10px空隙; 64px强对比=浅底+主色块(不加细节)
"""
import os
from PIL import Image, ImageDraw, ImageFont

CANVAS = 512
OUT = os.path.dirname(os.path.abspath(__file__))
PORT = "8454"
MAIN = (0xBF, 0xE9, 0xF5)      # #BFE9F5 冰蓝白主色
BG = (0x0E, 0x11, 0x16)        # #0E1116 底色
TEAL = (0x3F, 0xB8, 0xA0)      # #3FB8A0 青绿点(家族固定,谁都不许换)
FONT = "/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf"
CX, CY = 256, 256
R_OUT, LINE_W = 150, 10        # 环外径(半径) / 线宽
SCAN_A = 20                    # 扫描线透明度 <=25

# 虎鲸多边形: 中心(256,364), 框 x[208,304] y[344,384]
# 与环内边缘(半径140→y=396)留 12px 空隙 (>=10); 不压数字(数字 x<=365, 虎鲸 x>=214 但 y>=344 低于数字中心带)
BODY = [
    (224,356),(236,352),(246,350),(256,344),(266,350),(278,354),(286,358),
    (292,350),(300,362),(300,370),(292,378),(286,372),(276,376),(256,380),
    (238,376),(226,372),(220,364),(214,358),
]
WHITE_SPOTS = [               # 白斑/腹(底色对比, 64px下主色块+深色斑强对比)
    (238,360,250,372),        # 眼后白斑
    (236,372,278,382),        # 腹
]


def base_canvas():
    """底色 + 每4px一条横向扫描线(比底色亮一点, alpha<=25)"""
    img = Image.new("RGBA", (CANVAS, CANVAS), BG + (255,))
    ov = Image.new("RGBA", (CANVAS, CANVAS), (0, 0, 0, 0))
    d = ImageDraw.Draw(ov)
    scan = tuple(BG[i] + (255 - BG[i]) * SCAN_A // 255 for i in range(3))
    for y in range(0, CANVAS, 4):
        d.line([(0, y), (CANVAS - 1, y)], fill=scan + (SCAN_A,))
    return Image.alpha_composite(img, ov)


def ring(img):
    """开环: 圆心(256,256) 外径150 线宽10, 右上缺40°(主色)
    PIL 角度以3点为0°、顺时针为正; 缺口中心315°(右上), 半宽20° → start=335, end=295+360"""
    d = ImageDraw.Draw(img)
    bbox = (CX - R_OUT, CY - R_OUT, CX + R_OUT, CY + R_OUT)
    d.arc(bbox, start=335, end=295 + 360, fill=MAIN + (255,), width=LINE_W)
    return img


def port(img):
    """环内四位端口号: 自适应缩字号, 总宽<=内径*0.78; 居中, 主色"""
    d = ImageDraw.Draw(img)
    inner_diam = 2 * (R_OUT - LINE_W)        # 2*(150-10)=280
    limit = inner_diam * 0.78                # 218.4
    dummy = ImageDraw.Draw(Image.new("RGB", (10, 10)))
    best = 10
    for s in range(200, 10, -1):
        f = ImageFont.truetype(FONT, s)
        w = dummy.textbbox((0, 0), PORT, font=f)[2] - dummy.textbbox((0, 0), PORT, font=f)[0]
        if w <= limit:
            best = s
            break
    f = ImageFont.truetype(FONT, best)
    d.text((CX, CY), PORT, font=f, anchor="mm", fill=MAIN + (255,))
    return img


def bars(img):
    """开环右侧三条4x26竖纹, 间距10, 主色, alpha 200/120/60"""
    d = ImageDraw.Draw(img)
    bw, bh = 4, 26
    left = 424 - 16                        # 环外右边缘406 + 空隙; 中心424
    y0 = CY - bh // 2
    for i, a in enumerate((200, 120, 60)):
        x = left + i * (bw + 10)
        d.rectangle([x, y0, x + bw - 1, y0 + bh - 1], fill=MAIN + (a,))
    return img


def cursor(img):
    """下缘往上4x40光标竖线(x=256), 主色, alpha 0(顶)→180(底)"""
    d = ImageDraw.Draw(img)
    top, bot = 472, 511
    for y in range(top, bot + 1):
        a = int(180 * (y - top) / (bot - top))
        d.line([(254, y), (257, y)], fill=MAIN + (a,))
    return img


def teal_dots(img):
    """环外左上两枚直径8 #3FB8A0 青绿点, 间距14, 与环留>=10px"""
    d = ImageDraw.Draw(img)
    for (x, y) in ((132, 132), (146, 132)):
        d.ellipse((x - 4, y - 4, x + 4, y + 4), fill=TEAL + (255,))
    return img


def whale(img):
    """徽记: 虎鲸剪影(背鳍+眼后白斑). 64px强对比=浅底+主色块+深色斑"""
    hal = Image.new("RGBA", (CANVAS, CANVAS), (0, 0, 0, 0))
    hd = ImageDraw.Draw(hal)
    hd.ellipse((221, 329, 291, 399), fill=MAIN + (70,))   # 浅底(主色浅圆)
    img = Image.alpha_composite(img, hal)
    d = ImageDraw.Draw(img)
    d.polygon(BODY, fill=MAIN + (255,))                   # 主色块主体
    d.polygon(BODY, outline=MAIN + (255,))                # 主色发光轮廓
    mask = Image.new("L", (CANVAS, CANVAS), 0)
    ImageDraw.Draw(mask).polygon(BODY, fill=255)
    wl = Image.new("RGBA", (CANVAS, CANVAS), (0, 0, 0, 0))
    wd = ImageDraw.Draw(wl)
    for b in WHITE_SPOTS:
        wd.ellipse(b, fill=BG + (255,))                   # 白斑/腹=底色对比
    wl = Image.composite(wl, Image.new("RGBA", (CANVAS, CANVAS), (0, 0, 0, 0)), mask)
    img = Image.alpha_composite(img, wl)
    return img


def main():
    img = base_canvas()
    img = ring(img)
    img = port(img)
    img = bars(img)
    img = cursor(img)
    img = teal_dots(img)
    img = whale(img)
    rgb = img.convert("RGB")
    rgb.save(os.path.join(OUT, "avatar_512.png"))
    for sz in (40, 64, 128, 256):
        rgb.resize((sz, sz), Image.LANCZOS).save(os.path.join(OUT, f"avatar_{sz}.png"))
    print("OK 生成 avatar_512.png + avatar_{40,64,128,256}.png")


if __name__ == "__main__":
    main()
