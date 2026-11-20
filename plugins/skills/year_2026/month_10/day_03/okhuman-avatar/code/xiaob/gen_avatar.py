# 小B 8452 · 家族头像（电蓝 #2E9BE6 · 徽记=内存条：方芯片+金手指）
# 只用标准库 + Pillow；无随机、无网络；两次运行字节一致。
# v2（按小A 三条修正）：① 数字 textbbox 自适应缩字号，总宽 ≤ 开环内径×0.78；
#   ② 徽记与环留 ≥10px、64px 下用强对比（浅底+主色块）；③ 交付前重新运行生成。
from PIL import Image, ImageDraw, ImageFont

W = 512
BG   = (14, 17, 22)         # #0E1116
MAIN = (46, 155, 230)       # #2E9BE6 电蓝（本实例主色）
TEAL = (63, 184, 160)       # #3FB8A0 家族青绿点
GOLD = (212, 165, 52)       # 金手指
LIGHT= (226, 232, 239)      # 徽记浅底
FONT = '/usr/share/fonts/truetype/dejavu/DejaVuSansMono.ttf'
PORT = '8452'
INNER_DIA = 280.0            # 开环内径（内半径 140）
MAX_TEXT_W = INNER_DIA * 0.78

def build():
    img = Image.new('RGBA', (W, W), BG + (255,))
    d = ImageDraw.Draw(img)

    # 2. 开环：圆心(256,256) 外径150 线宽10，右上缺 40°（屏幕角315°居中，缺 295..335）
    #    Pillow 的 arc 线宽向内收：bbox 半径=150 → 外缘150 内缘140
    d.arc((106, 106, 406, 406), 335, 295, fill=MAIN+(255,), width=10)

    # 6. 主环左上两枚直径8青绿点（间距14，135°方向，环外缘外3px起）
    d.ellipse((141, 141, 149, 149), fill=TEAL+(255,))
    d.ellipse((131, 131, 139, 139), fill=TEAL+(255,))

    # 3. 环内端口号：自适应缩字号，总宽 ≤ 内径×0.78，居中（字高约66）
    size = 130
    while True:
        f = ImageFont.truetype(FONT, size)
        bb = d.textbbox((0, 0), PORT, font=f)
        if bb[2] - bb[0] <= MAX_TEXT_W:
            break
        size -= 1
    d.text((256, 256), PORT, font=f, fill=MAIN+(255,), anchor='mm')

    # 个性化徽记：内存条（方芯片+金手指）
    #   中心(256,356)=372-16，外框91×41；角距129.5 → 与环内缘140留10.5px
    #   浅底+主色块强对比，64px 下仍是"浅条+蓝块+金边"
    d.rectangle((211, 336, 301, 376), fill=LIGHT+(255,), outline=MAIN+(255,), width=2)  # 板
    d.rectangle((244, 338, 269, 363), fill=MAIN+(255,))                                # 方芯片
    d.rectangle((248, 342, 252, 346), fill=LIGHT+(255,))                               # die 标记
    d.rectangle((211, 371, 301, 376), fill=GOLD+(255,))                                # 金手指带
    d.rectangle((254, 371, 260, 376), fill=LIGHT+(255,))                               # 中间缺口
    for x in range(215, 300, 4):                                                       # 手指分段
        d.line([(x, 371), (x, 376)], fill=LIGHT+(255,))

    # 4. 开环右侧三条 4×26 竖纹（间距10，alpha 200/120/60）
    bars = Image.new('RGBA', (W, W), (0, 0, 0, 0))
    db = ImageDraw.Draw(bars)
    for x0, a in ((410, 200), (424, 120), (438, 60)):
        db.rectangle((x0, 243, x0+3, 268), fill=MAIN+(a,))
    img = Image.alpha_composite(img, bars)

    # 5. 下缘 4×40 主色渐变竖线（终端光标）：x=256，y 472..511，alpha 底0→顶180
    cur = Image.new('RGBA', (W, W), (0, 0, 0, 0))
    dc = ImageDraw.Draw(cur)
    for y in range(472, 512):
        a = round(180 * (511 - y) / 39)
        dc.rectangle((254, y, 257, y), fill=MAIN+(a,))
    img = Image.alpha_composite(img, cur)

    # 1. 整幅每4px一条横向扫描线（白 alpha20，≤25）
    sc = Image.new('RGBA', (W, W), (0, 0, 0, 0))
    ds = ImageDraw.Draw(sc)
    for y in range(0, W, 4):
        ds.line([(0, y), (W-1, y)], fill=(255, 255, 255, 20))
    img = Image.alpha_composite(img, sc)
    return img.convert('RGB')

def main():
    rgb = build()
    rgb.save('avatar_512.png')
    for n in (256, 128, 64, 40):
        rgb.resize((n, n), Image.LANCZOS).save(f'avatar_{n}.png')

if __name__ == '__main__':
    main()
