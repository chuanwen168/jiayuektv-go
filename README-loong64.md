# 家悦K歌 · 龙芯（LoongArch）设备（Docker）部署包

适用于 **linux/loong64** 设备：龙芯 3A5000 / 3A6000 等 LoongArch64 架构的 NAS、迷你主机、桌面机。

本包已含交叉编译好的 `linux/loong64` 静态二进制，设备上构建**无需 Go 工具链**，Docker 自动拉取 loong64 基础镜像（Debian bookworm 官方支持 LoongArch）。

## 快速开始

```bash
# 1. 解压到设备（推荐 /vol1/1000/docker/ktvhome 或 /home/ktvhome）

# 2. 建目录
cd /vol1/1000/docker/ktvhome
mkdir -p data mv mv-net singer

# 3. 构建并启动（龙架构专用 compose）
sudo docker compose -f docker-compose.loong64.yml up -d --build

# 4. 看日志确认启动
sudo docker logs -f jiayue-ktv-go
```

> ⚠️ **龙芯设备一般没有 `/dev/dri` 渲染节点**：先编辑 `docker-compose.loong64.yml`，把 `devices:` 段落删掉再启动，程序自动回退软件编码（CPU 占用会比 x86+核显高，正常）。若你的设备确有 `/dev/dri`（部分龙芯主机带核显）可保留。

## 使用

浏览器访问 **http://设备IP:8086**

| 页面 | 地址 |
| --- | --- |
| 主页 | `http://设备IP:8086/` |
| TV 播放端 | `http://设备IP:8086/tv/` |
| PAD 点歌台 | `http://设备IP:8086/pad/` |
| 手机点歌 | `http://设备IP:8086/m/` |
| 曲库管理 | `http://设备IP:8086/admin/`（默认密码 `admin888`，请修改） |

## 曲库

- 本地歌曲放 `mv/`（每子文件夹一个曲库，如 `mv/华语`）
- 网盘歌曲放 `mv-net/`
- 歌手头像放 `singer/`（`歌手名.jpg`）
- 新曲库需进后台「曲库管理 → 曲库来源」启用
- 首次使用：后台 → 🔄 扫描曲库

> 版本：家悦K歌 1.0.1（Go 版）。本包前端为源码（未混淆）。歌曲版权归原版权方所有。
