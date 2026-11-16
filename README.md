<div align="center">
  
# xdnmb-stat
</div>
<p align="center">
  <img src="sage.gif" alt="sage">
</p>

从访问者自己的浏览器检测 X 岛接口和图片加载情况。结果只代表当前网络下的一次抽样，不代表所有地区、所有图片或上传功能均正常。

## 检测方式

- 图床：2 张固定样本，加上从帖子及回复中选出的最近 7 天内的 3 张不同图片；分别加载缩略图和原图。读取当前 CDN 配置，去重后最多覆盖 3 个地址，图片并发上限为 4。
- 接口：分别请求 CDN 配置和帖子列表，检查 HTTP 状态、JSON 解析和内容结构；公告接口也检查实际内容。
- 任何样本失败都不会显示全绿；近期样本不足、CDN 配置无法取得、跨域不可读或覆盖不完整时明确显示“无法验证”。成功文案为“本轮抽样检测通过”。
- 每 5 分钟自动更新，手动检测间隔至少 30 秒；单个请求最多 10 秒。失败原因只描述观察结果，不直接归因于 DNS。

## 为什么需要可选浏览器脚本

截至 2026-11-16，测试的 X 岛接口没有向状态页开放 CORS。普通网页无法读取它们的 HTTP 状态和 JSON；`no-cors` 也不能解决这个问题。服务器代理虽然可以读取，但会测量服务器的网络，不能代表访问者。

因此，默认模式仍由浏览器直接发起请求，不能读取时显示“无法验证”。访问者可自行在 Tampermonkey 等脚本管理器中安装 [local-probe.user.js](local-probe.user.js) 并刷新页面。脚本通过访问者浏览器发起匿名请求，再把结果交给页面验证。**没有任何接口健康结果来自服务器。** 浏览器、系统或扩展配置的代理仍会影响该访问者实际使用的网络路径；这一请求方式使用浏览器自身网络栈，见 [Tampermonkey 官方说明](https://www.tampermonkey.net/documentation.php?locale=en&q=GM_xmlhttpRequest)。

脚本只允许本页所需的 3 个 API 域名的固定公开接口及公告接口，不支持任意目标代理，不发送账号 Cookie。默认匹配 `https://status.kusotech.club/*`、GitHub Pages 地址和本机 18787 端口；使用其他域名时需要添加对应的精确 `@match`。没有安装脚本的访问者仍能使用图片检测。

## 部署样本目录服务

服务已改为 Go，页面和脚本均内嵌在单个可执行文件中，无第三方依赖。服务器不需要安装 Node.js 或 Go，也不需要单独放置前端文件。服务只提供近期图片路径和 CDN 地址，不提供服务器侧的健康状态；图片仍由访问者浏览器直接加载。

选择与服务器架构匹配的 Linux 包：`linux-amd64` 对应常见的 x86_64，`linux-arm64` 对应 aarch64。解压后运行：

```sh
chmod +x xdnmb-stat
./xdnmb-stat
```

默认访问 `http://127.0.0.1:18787`。用 `XDAO_HOST`、`XDAO_PORT` 环境变量调整监听地址与端口。公网部署可由现有 HTTPS 反向代理转发到本机 18787；服务同时提供页面及 `/image-samples.json`，没有通用请求代理端点。程序可以在任意工作目录启动。服务需要系统现有的 CA 根证书来验证 X 岛 HTTPS 连接。

`status.kusotech.club` 使用独立的 Nginx HTTPS 入口，配置见 [Nginx 配置](deploy/status.kusotech.club.conf)。先让该域名解析到服务器，并取得覆盖该域名的证书，再启用此配置。不要向公网开放内部端口；8787 留给服务器原有的 `/acp/` 服务。旧部署如有 `listen.conf` 覆盖配置，需要将其中的 `XDAO_HOST` 改为 `127.0.0.1`，并明确设定 `XDAO_PORT=18787`。

使用 Certbot webroot 模式续期时，保留配置中的 `/.well-known/acme-challenge/` 路由，并将 [续期重载脚本](deploy/renew-status-certificate.sh) 以可执行权限安装到 `/etc/letsencrypt/renewal-hooks/deploy/xdnmb-stat-nginx-reload`，使新证书在续期后自动被 Nginx 加载。

如果使用 systemd，可将程序放在 `/opt/xdnmb-stat/xdnmb-stat`，将 [部署文件](deploy/xdnmb-stat.service) 放到 `/etc/systemd/system/xdnmb-stat.service`，然后执行：

```sh
sudo systemctl daemon-reload
sudo systemctl enable --now xdnmb-stat
```

默认设置 Go 运行时的 32 MiB 内存软目标，可通过 `GOMEMLIMIT` 覆盖。它会影响垃圾回收频率，**不是进程实际内存的硬上限**（[Go 官方说明](https://pkg.go.dev/runtime/debug#SetMemoryLimit)）；实际占用会随系统和访问量变化。上游响应限制为 2 MiB，只保留少量图片地址，最多同时处理 64 个页面请求。没有定时后台轮询，无访问时不会持续向 X 岛发请求。

2026-11-16 本地 Windows amd64 实测：启动并访问页面后工作集约 9.02 MiB，取得真实近期样本后约 15.77 MiB；缓存命中约 2 ms。这是一次本机冒烟检查，不是 Linux 生产环境的内存保证或负载测试。详细记录保留在本地 `validation/smoke-memory.json`。

目录按需更新并缓存 5 分钟，同时到来的请求复用一次更新；更新期间可立即返回未过期的旧目录，旧时间戳保持不变。更新失败时最多保留 15 分钟，过期返回 503。目录只含生成时间、CDN 地址和最多 3 个图片路径，不保存帖子正文或用户信息。样本服务不能访问 X 岛时，页面会报告覆盖不足，而不是把访问者的图床判为宕机。

也可以继续用 GitHub Pages 托管前端，并在 `index.html` 加载 `status-page.js` **之前**配置目录服务：

```html
<script>window.XDAO_SAMPLE_CATALOG = 'https://你的样本服务域名/image-samples.json';</script>
```

样本接口允许公开跨域读取。没有目录服务且浏览器又无法读取帖子接口时，只能检测固定样本，页面会显示检测不完整。安装浏览器脚本后，如果能取得足够近期图片，也无需目录服务。

## 验证

仅开发机器需要 Go 1.22+；前端回归检查使用 Node.js，两者都不需要安装到部署服务器。

```sh
go test ./...
go vet ./...
node --test --test-isolation=none tests/status.test.cjs tests/page.test.cjs
```

自行交叉编译 Linux 版本（在开发机器执行，构建时页面会一起嵌入）：

```sh
CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o xdnmb-stat .
# ARM64 服务器将 GOARCH 改为 arm64
```

覆盖 HTTP 错误、错误页、无效 JSON、跨域不可读、超时、近期图片筛选、缩略图与原图差异、部分失败、目录过期、并发限制、页面汇总及浏览器脚本请求桥接。浏览器脚本仍需在实际部署域名和所用脚本管理器中完成安装验证。
