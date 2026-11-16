# X 岛服务状态

[status.kusotech.club](https://status.kusotech.club/)：服务器定期探测 X 岛接口和图床，公开最近状态、30 / 90 天历史和自动异常事件。访问页面只读取已有记录，不会触发检测，无需安装浏览器脚本。

**结果代表台湾 Google Cloud 单个监测节点与 X 岛之间的连接，不代表访问者的网络，也不是 X 岛官方状态页。** GitHub Pages 从 `main` 分支的 `/docs` 目录发布独立入口页，展示随机配图，点击按钮前往上述站点。

## 探针与记录

- 默认每 5 分钟检测一次，无人访问也会运行；探针不重叠，单轮最多 90 秒，接口和缩略图请求最多 10 秒，原图完整下载最多 30 秒。
- 主站、备用域名、备用 API：分别读取 CDN 配置和帖子列表，验证 HTTP 状态、JSON 和内容结构；另检测公告接口。
- 跟随同源或已知 X 岛 API 域名之间的 HTTPS 跳转（最多 5 次），验证最终响应。正常的 301 / 302 跳转本身不算故障；超出允许范围时明确显示无法确认。
- 图床：从当轮帖子和回复中选取最近 7 天内的 3 张不同图片，另加 2 张固定样本；分别测试缩略图和原图，去重后最多覆盖 3 个动态 CDN 地址。旧图成功不能掩盖近期图片失败；配置或近期样本不足时显示未知。
- 图片检查 HTTP 状态、图片类型和图片头尺寸，并流式读完整个响应。不将图片全部解码到内存；不测试上传功能。最多并发下载 3 张，每张上限 10 MiB，超出上限记为无法确认。
- HTTP 错误、格式无效、下载中断或部分样本失败才记为故障；接口和缩略图超过 3 秒、原图完整下载超过 10 秒但验证成功，只显示蓝色“较慢但可用”，不触发故障事件。图片达到下载时限或本轮探针中断时记为未知，明确提示未完成验证。只描述观测到的错误，不直接归因于 DNS 污染。
- 每轮结果先写入磁盘，再对外显示。按北京时间分日保存 JSONL，保留最近 90 天；服务重启会读取已有记录。磁盘写入失败、监测超时未更新时，不继续展示全绿。
- 可用率是可判定记录中全部验证成功的比例，包含较慢但成功的记录，部分失败不算可用；该比例不等同于精确的持续可用时间。未知记录不参与可用率；缺失记录通过覆盖率和斜纹标识。尚未开始监测的历史保持灰色。
- 自动记录故障开始与恢复；慢响应仅记录性能，监测中断不会被当作恢复。页面列出保留期内最近 500 条故障事件，原始记录仍按天保存。页面每分钟同步记录，30 / 90 天按钮仅切换展示范围。
- 读取旧版历史时，对明确记录了“全部样本通过”的慢响应重新分类，原始日志不改写；真实失败或覆盖不足的记录保持原状。

## Linux 部署

单个 Go 可执行文件内嵌页面，无第三方依赖；**服务器不需要安装 Node.js 或 Go**，只需要系统 CA 根证书。可在开发机器编译后通过 `scp` 上传，不依赖 GitHub Release。

将 Linux amd64（x86_64）或 arm64（aarch64）可执行文件放到 `/opt/xdnmb-stat/xdnmb-stat`，赋予执行权限，将 [systemd unit](deploy/xdnmb-stat.service) 放到 `/etc/systemd/system/xdnmb-stat.service`：

```sh
sudo chmod 755 /opt/xdnmb-stat/xdnmb-stat
sudo systemctl daemon-reload
sudo systemctl enable --now xdnmb-stat
sudo systemctl status xdnmb-stat --no-pager
curl -fsS http://127.0.0.1:18787/api/status
```

该 unit 自动创建持久目录 `/var/lib/xdnmb-stat` 并赋予动态服务用户写入权限。升级只替换程序和 unit，然后 `systemctl daemon-reload && systemctl restart xdnmb-stat`；**保留并备份数据目录**，否则历史会丢失。不要运行多个实例共同写同一目录。

服务只监听 `127.0.0.1:18787`，由 Nginx 提供公网 HTTPS。[Nginx 配置](deploy/status.kusotech.club.conf) 使用 `status.kusotech.club` 的证书；首次部署需要先配置 DNS、申请相应证书，再启用配置。不要开放内部端口；8787 留给服务器原有服务。

Certbot webroot 续期需保留 `/.well-known/acme-challenge/` 路由，并将 [证书续期重载脚本](deploy/renew-status-certificate.sh) 以执行权限安装到 `/etc/letsencrypt/renewal-hooks/deploy/xdnmb-stat-nginx-reload`。已有服务的 `listen.conf` 覆盖配置也必须使用 `127.0.0.1:18787`。

可通过 systemd override 调整环境变量：

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `XDAO_HOST` | `127.0.0.1` | 监听地址 |
| `XDAO_PORT` | `18787` | 内部端口 |
| `XDAO_INTERVAL_SECONDS` | `300` | 检测间隔，允许 60–3600 秒 |
| `XDAO_DATA_DIR` | `data` | 直接运行时相对当前目录；unit 设为 `/var/lib/xdnmb-stat` |
| `XDAO_PROBE_LOCATION` | `台湾 · Google Cloud` | 页面显示的探针位置，迁移服务器时同步修改 |
| `GOMEMLIMIT` | `32MiB` | Go 内存软目标，并非进程内存硬上限 |

只在内存中保留每日汇总、最近状态和有限事件，启动时逐行读取日志。API JSON 响应上限 2 MiB，图片流式校验，网页请求并发上限 32；不保存图片文件或帖子正文。磁盘占用取决于轮数与错误详情，原始日志会自动清理过期日期文件。

## 本地开发与验证

页面沿用 kusotech 主页的配色与传统控件样式。已安装的设备优先使用 MS Gothic；其他设备自动加载随站点提供的 Noto Sans Mono CJK SC 衍生字体，不需要安装字体。它是相近的 Gothic 替代字体，并非 MS Gothic 原版。字体仅包含界面与内置检测消息用字，其他字符回退到设备字体；许可和来源见 `docs/assets/fonts/`。修改界面文字后可用 `scripts/build_webfont.py` 从原始 OTF 重新生成，并递增脚本内的字体文件版本以更新浏览器缓存；需要 Python 的 `fonttools[woff]`，服务器不需要这些构建工具。

仅开发机器需要 Go 1.22+；可选的前端逻辑检查需要 Node.js：

```sh
go test ./...
go vet ./...
node --test --test-isolation=none tests/page.test.cjs
go run .
```

本地访问 `http://127.0.0.1:18787`；首次启动立即开始后台探针，完成前显示等待首次检测。只读端点 `/api/status` 返回状态、每日汇总和异常事件；没有手动探测或通用代理端点。

在开发机器交叉编译（以下为 Linux / macOS shell）：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o xdnmb-stat .
# ARM64 服务器将 GOARCH 改为 arm64
```

测试覆盖真实格式校验、旧图成功但近期原图失败、未知状态聚合、日志损坏与写入失败、跨重启历史、保留期清理、故障恢复与监测中断、无人访问的定时执行，以及只读页面不会触发探针。
