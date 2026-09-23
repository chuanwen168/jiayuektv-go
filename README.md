# 家悦K歌 局域网点歌系统 — 飞牛系统（fnOS）部署指南

Go 语言重写版服务端（对齐 1.2.0 功能），前端四端（TV 播放端 / PAD 点歌台 / 手机点歌 / 曲库管理）不变。

本包已含交叉编译好的 `linux/amd64` 静态二进制，飞牛（x86）上构建无需 Go 工具链。

## 一、把项目放到飞牛

用飞牛的文件管理或 SCP 把 `ktvhome-fnos.zip` 解压到飞牛磁盘，

推荐路径：`/vol1/1000/docker/ktvhome`（`/vol1` 是你的存储卷，按实际修改）。

SSH 到飞牛（飞牛设置 → 终端 / SSH 开启）：



```
cd /vol1/1000/docker

unzip ktvhome-fnos.zip -d ktvhome

cd ktvhome

\# 创建数据/曲库/头像目录（以 admin 身份创建，方便后续拷文件）

mkdir -p data mv mv-net singer
```

## 二、配置曲库（1.2.0 多曲库）



| 挂载点             | 用途              | 示例                           |
| --------------- | --------------- | ---------------------------- |
| `./mv/<名称>`     | 本地曲库（每个子目录一个来源） | `mkdir mv/huayu`，MV 放进去      |
| `./mv-net/<名称>` | 网盘曲库            | `mkdir mv-net/pan`，网盘挂载 / 拷入 |
| `./singer`      | 歌手头像（图片名 = 歌手名） | `周杰伦.jpg`、`周杰伦&林俊杰.jpg`      |

文件名建议 `歌手 - 歌名.mp4`；多歌手用 `&`：`歌手1&歌手2 - 歌名.mp4`。
支持 `歌手-歌名-语种-风格.mp4`（如 `郭静-心墙-国语-流行歌曲.mkv`）自动识别语种/风格；无歌名时也支持 `歌手-语种-风格.mp4`。

**挂载新目录后，还需在后台「曲库管理 → 曲库来源」里把对应来源启用**，才会参与扫描。

## 三、构建并启动



```
\# 有核显/独显（推荐，VAAPI 硬件转码）：

sudo docker compose -f docker-compose.fnos.yml up -d --build

\# 无核显/独显：先删掉 docker-compose.fnos.yml 里 devices 段，再执行上面命令。
```

查看日志：`sudo docker logs -f jiayue-ktv-go`

硬件加速确认：日志出现 `核显调用: 成功` / `VAAPI 驱动信息` 即 VAAPI 生效；

若出现 `未检测到渲染节点` 则走了软件编码（CPU 占用高）。

## 四、使用



| 页面       | 地址                        |
| -------- | ------------------------- |
| 主页（四个入口） | `http://飞牛IP:8084/`       |
| TV 点歌屏   | `http://飞牛IP:8084/tv/`    |
| PAD 点歌     | `http://飞牛IP:8084/pad/`   |
| 手机点歌     | `http://飞牛IP:8084/m/`     |
| 曲库管理     | `http://飞牛IP:8084/admin/` |

首次部署后，在后台或 TV 端点「扫描曲库」，或：`curl -X POST http://飞牛IP:8084/api/scan`

## 五、1.2.0 新功能说明



* **预置管理员密码**：compose 里 `ADMIN_PASSWORD=admin888` 会在首次部署时自动

  初始化后台密码（仅从未设置过密码时生效，**记得改成你自己的密码**；改过后

  以后台「修改密码」为准）。

* **多曲库**：/mv 与 /mv-net 下每个子目录是一个 "曲库来源"，后台可独立启用 /

  停用；停用的来源不会被扫描、也不会被清理，重启用后自动恢复。

* **歌手头像**：/singer 下放 `歌手名.jpg` 即可，TV 歌手列表 / 手机端 / 后台自动

  展示；没有头像时自动回退首字母色块。

* **NVENC（NVIDIA 显卡）**：在 compose 打开 `runtime: nvidia` + 两个 NVIDIA 环境

  变量注释，代码自动探测 `h264_nvenc`（注意 Debian 官方 ffmpeg 不含 nvenc，

  需使用带 nvenc 的 ffmpeg 构建，否则自动回退 VAAPI/libx264）。

* **多歌手拆分**：`歌手1&歌手2` 会拆成两位独立歌手（如 `高进&小沈阳` 可在歌手

  列表分别点入），按任一歌手名都能找到这首歌；修改歌手时用 `&` 分隔即可。

* **语种 / 风格**：曲库管理表格里可直接下拉修改每首歌的语种、风格，也支持

  勾选多首批量设置；预设（语种：国语/粤语/英语/日语/韩语/其他；风格：流行/

  摇滚/怀旧/儿歌/民谣/舞曲/合唱/DJ/其他）可在批量设置面板里增删自定义项。

* **PAD 点歌**：新增平板点歌页（动感韶音主题），左侧大画面播放、右侧点歌

  列表，适合放包房点歌台；主页已加入口。

## 六、常见问题



* **集显没生效 / CPU 高**：确认宿主机有 `/dev/dri`（`ls /dev/dri/` 应有

  renderD128），compose 里 `devices` 段未被删，重建容器

  `sudo docker compose -f docker-compose.fnos.yml up -d --force-recreate`；

  代码会自动探测 renderD128/renderD129/card0 等候选节点。

* **换端口**：改 compose `ports: "8084:8080"` 左侧宿主机端口。

* **HLS 缓存清理**：默认 3 天，改环境变量 `HLS_CACHE_MAX_AGE_DAYS`。

* **从源码重新构建**：把 compose 的 `dockerfile: Dockerfile.single` 改为

  `Dockerfile`（多阶段构建，需能访问 Docker Hub 拉取 golang 镜像）。

* **升级**：替换新版本文件后 `sudo docker compose -f docker-compose.fnos.yml`

  `up -d --build`，数据库与曲库在 `./data`、`./mv` 中持久化，不受影响。