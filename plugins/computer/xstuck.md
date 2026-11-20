# computer — 桌面像"卡住"了：先查谁在 grab

**症状**：桌面/面板右键**弹不出菜单**、"键盘一直在输入"却进不了应用、对某些窗口的点击**没反应也不报错**（xusectl 警告"指针下顶层窗口与激活窗口不一致"）。杀 xclip、重启 xfdesktop、补 mouseup 都没用。

**真凶**：一个**已 map 的 XFCE 应用菜单**（菜单对指针和键盘做 grab，挂着不 map-out，全桌面的右键和按键都被它吃掉）。

```bash
xwininfo -root -tree | grep -i whisker                    # 看到 0x… "Whisker Menu" ("wrapper-2.0")
xdotool search --name "Whisker Menu" | xargs -r xdotool windowunmap
```

**判据**：右键无菜单 + 键盘有"输入"却进不了应用 → 先 `xwininfo -root -tree` 找 map 着的菜单/弹出层（`Whisker Menu`、`wrapper-2.0`、各种 `menu`），unmap 它，别急着重启桌面组件或注销。
