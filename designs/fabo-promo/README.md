# Fabo 宣传片

30 秒，1920×1080，30 fps，中文标题，原创合成器配乐，无旁白。

- `fabo-promo-v2-1080p.mp4`：当前 H.264 / AAC 成片。旧版为 `fabo-promo-1080p.mp4`。
- `index.html`：可编辑动画源文件，带播放和拖动进度控件。
- `assets/fabo-window.png`：来自仓库 docs/images/fabo-window.png 的应用截图；镜头推近为后期动画，并非操作录屏。
- `assets/logo.png`：现有 Fabo 品牌图。
- `music.py`：原创配乐生成器，无外部音乐采样。
- `vendor/`：本地 React、Babel 和 baoyu-design animations-v3 / tweaks-panel，保留原始组件注释。源页面不需要 CDN。

## 预览与导出

从仓库根目录执行 `python3 -m http.server 4311 --directory designs`，访问 http://localhost:4311/fabo-promo/ 。

导出使用 Ego 浏览器驱动 animations-v3 时间轴，以 30 fps 截取 900 张 1080p 帧，再用 FFmpeg 编码。工具要求：ego-browser、Python 3、FFmpeg。执行：

```sh
ego-browser nodejs < designs/fabo-promo/render.mjs
python3 designs/fabo-promo/music.py
sh designs/fabo-promo/encode.sh
```

帧暂存在 `/tmp/fabo-promo-frames/`。页面播放为动画预览，配乐在 MP4 中合成。修改脚本后需要重新导出；字体依赖 macOS 系统字体，其他系统可能存在字形差异。

宣传内容依据当前 README 中的 Fabo 功能描述；未采用上游 Kiri 性能数据。命令画面为用法展示，不是本次执行记录。

## v2：快节奏功能版

30 秒、11 个镜头，开场从 4 秒缩至 2 秒，收尾从 6 秒缩至 3 秒。120 BPM 鼓点，镜头切换在整拍；入场改为 0.22 秒。新增容器启动命令、资源管理、日志、交互终端、Kubernetes、Linux machine。

性能调查：2026-11-13 的 `raw-final.log` 记载单次 VM boot 177.7 ms，`bench-api.log` 记载容器完整生命周期 API p50 31.01 ms（network none，5 次测试与 5 次预热）。这些与上游 README 的 30 次测试不是同次同环境对比。最终宣传片不展示速度数字或性能领先声明，启动镜头仅为命令用法示意。`evidence/raw-final.log` 仅保留调查证据，不作为成片宣传依据。本次未重新运行性能基准。

新增画面来自 `target/desktop-streams-check/native-screenshots/` 的 container-tty / container-live-logs，以及 `target/desktop-advanced-check/native-screenshots/service-details.png`。保留验收原图；功能字幕为宣传编辑，非实时操作。

原版 MP4 在 `v1/`，同目录旧 HTML 仅作版本存档；若需恢复，将旧 HTML 复制到项目根目录以使用共享 assets/vendor。
