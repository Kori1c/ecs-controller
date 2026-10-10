# ECS 控制台

一个阿里云 ECS 管理面板，可以查看实例状态和流量，并按设置在流量达到阈值时自动关机。

> 阿里云流量数据可能存在延迟，自动关机不能保证绝对不会产生额外费用。请保留余量，并关注面板中的异常提示。

## 主要功能

- 查看实例状态、规格、公网 IP 和流量。
- 同时查看每台实例的流量（CMS）和账号流量（CDT），支持流量超限告警或自动关机。
- 创建、启动、停止和释放 ECS，以及更换由面板创建并管理的弹性公网 IP（EIP）。
- 定时开关机、停机保活和每月自动开机。
- Telegram、邮件、接口回调（Webhook）通知和每日流量摘要，支持 Telegram 远程控制。
- 自动将公网 IP 同步到 Cloudflare 域名（DDNS）。
- 查看余额和消费账单，支持 Passkey 登录、自定义名称和 Logo。
- 加密数据备份与恢复；恢复前请阅读[当前限制](#备份与恢复)。

## 部署

准备一台 Linux 服务器，安装并启动 **Docker（含 Docker Compose）**，同时安装 **Git 和 curl**。当前用户需要有运行 Docker 的权限。

### 1. 安装服务

在服务器上执行：

```bash
curl -fsSL https://raw.githubusercontent.com/Kori1c/ecs-controller/main/install.sh \
  | env ECS_COOKIE_SECURE=0 bash
```

安装脚本默认将项目放到 `~/ecs-controller`，下载现成镜像并启动服务，不会在服务器上编译。

### 2. 开启 IP 访问

服务默认只允许服务器本机访问。在安装目录的 [docker-compose.yml](./docker-compose.yml) 中，将端口映射 `"127.0.0.1:43211:8080"` 改为 `"43211:8080"`，其他内容保持不变，然后在服务器上执行：

```bash
cd ~/ecs-controller
ECS_COOKIE_SECURE=0 ./deploy.sh
```

在服务器的云安全组中放行 TCP `43211`，建议只允许自己的 IP 访问。初始化时以这次启动显示的 `Setup token` 为准。

### 3. 打开面板

在浏览器访问 **`http://服务器IP:43211`**，将“服务器IP”替换为实际地址。

**建议正式使用时配置 HTTPS 域名访问，避免密码和账号信息以明文传输。**使用 HTTPS 时应启用 `ECS_COOKIE_SECURE=1`；Passkey 登录需要证书有效的 HTTPS 域名。

修改端口映射会产生本地部署配置改动，当前在线更新会拒绝覆盖这类改动，详见[在线更新](#在线更新)。

## 第一次使用

1. 保存安装脚本最后显示的 `Setup token`（初始化 token）。
2. 在浏览器打开面板，输入 token 并设置管理员密码。系统最低要求 6 位，建议使用较长、独立的密码。
3. 按下面的[权限说明](#阿里云权限)准备阿里云子账号，再进入“账号管理”添加账号、区域和可用流量。
4. 点击“测试账号”，按各项结果补充权限，再保存账号。系统会同步所选区域下的实例。
5. 进入“系统设置 → 流量保护”设置阈值和动作，在“通知中心”配置通知并发送测试消息。

### 初始化 token 忘记了怎么办

通过上述安装脚本部署时，在服务器上执行：

```bash
docker exec ecs-controller printenv ECS_SETUP_TOKEN
```

如果没有输出，说明服务可能自行生成了 token，再查看日志，并使用最近一次启动时显示的 token：

```bash
docker logs ecs-controller 2>&1 | grep '初始化 token'
```

token 只用于首次初始化，不是管理员登录密码，也不能用于重置已设置的管理员密码。不要公开 token 或包含 token 的日志。

## 阿里云权限

创建一个专门给本项目使用的 RAM 用户（阿里云子账号），不要使用主账号密钥，也不需要授予管理员权限。

### 怎么添加权限

1. 在阿里云控制台搜索并进入“访问控制 RAM”。
2. 创建用户，开启“OpenAPI 调用访问”，创建并保存 AccessKey ID 和 AccessKey Secret。
3. 给这个用户“添加权限”，选择“系统策略”，按下面的完整名称搜索并添加。
4. 将该用户的 AccessKey ID 和 AccessKey Secret 填入本面板的“账号管理”。

### 管理已有实例和监控流量

添加下面 3 项。名称可以直接复制搜索，点击名称可查看阿里云官方说明。

| 权限名称 | 用途 |
| --- | --- |
| [AliyunECSFullAccess](https://help.aliyun.com/zh/ram/developer-reference/aliyunecsfullaccess) | 查看和管理 ECS，支持开关机、流量超限自动关机和定时开关机 |
| [AliyunCloudMonitorReadOnlyAccess](https://help.aliyun.com/zh/ram/developer-reference/aliyuncloudmonitorreadonlyaccess) | 读取实例流量（CMS），以及 EIP 的地址和带宽 |
| [AliyunCDTReadOnlyAccess](https://help.aliyun.com/zh/ram/developer-reference/aliyuncdtreadonlyaccess) | 读取账号流量（CDT） |

注意：这是方便配置的方案，不是权限最少的方案。`AliyunECSFullAccess` 也允许创建和删除 ECS；只授予 ECS 只读权限则无法自动关机。

### 按需添加的权限

| 想使用的功能 | 额外添加的权限 |
| --- | --- |
| 在面板中创建实例，并自动创建网络 | [AliyunVPCFullAccess](https://help.aliyun.com/zh/ram/developer-reference/aliyunvpcfullaccess) |
| 创建实例时使用 EIP、更换 EIP，或释放实例时清理 EIP | [AliyunEIPFullAccess](https://help.aliyun.com/zh/ram/developer-reference/aliyuneipfullaccess) |
| 查看余额和消费账单 | [AliyunBSSReadOnlyAccess](https://help.aliyun.com/zh/ram/developer-reference/aliyunbssreadonlyaccess) |

不用对应功能时，可以不添加额外权限。**VPC 权限不包含 EIP 管理权限**，需要单独添加 EIP 权限。创建实例和 EIP 可能产生费用，请按需授权。

### 怎么看账号测试结果

- **通过**：本次查询或安全预检通过，不代表所有实例和所有功能都已授权。
- **缺少权限**：按该项显示的名称补充权限；已添加时，检查是否限制了区域或实例。
- **未验证**：暂时无法安全确认，不等于没有权限，不要为了消除提示而授予管理员权限。
- **检查失败**：可能是网络、服务未开通或账号信息问题，按提示处理后重试。

测试不会实际开关机、创建或删除云资源。当前区域没有实例，或接口不支持安全预检时，部分操作权限会显示“未验证”。

## 流量保护与自动化

“账号管理”里的**账号可用流量（GB）**是你为保护设置的额度，不是带宽，也不会自动购买流量包；请根据自己的阿里云套餐或预算填写。

“系统设置 → 流量保护”里的**告警阈值（%）**决定何时触发动作。例如可用流量填 **190 GB**，阈值设为 **95%**，约在 **180.5 GB** 时触发。

系统取可用的 CMS、CDT 流量中较大的值判断阈值，不把两者相加。选择“自动关机并告警”才会发起关机；选择“仅发送告警”不会关机。两种流量接口都不可用时，自动保护会暂停，请及时处理异常。

定时开关机在账号编辑窗口中设置；停机保活、每月 1 号自动开机和费用查询在“系统设置 → 运行与费用”中设置。保活只尝试启动仍存在的实例，不会自动重建已经被回收或释放的实例。

## 备份与恢复

进入“系统设置 → 备份恢复”，输入至少 8 位的备份密码，再点击“下载备份”。备份密码不会保存在系统中，请将备份文件和密码分开妥善保管。

恢复时选择备份文件并输入同一个密码。**恢复会覆盖当前账号、设置和历史数据，并清除登录会话**，完成后需要使用备份中的管理员凭据重新登录。

> **当前已知限制：**从另一套安装恢复后，服务重启会导致恢复的账号密钥无法读取。问题修复前，暂不建议使用网页恢复进行跨机器迁移。下载加密备份不受此限制。

更换域名时，原有 Passkey 不能直接沿用。迁移前应开启密码登录，并确认记得管理员密码。

## Passkey 登录

进入“系统设置 → 登录安全”，点击“设置 Passkey”，以后可使用指纹、面容、系统 PIN 或安全密钥登录。

注册和登录均使用证书有效的 HTTPS 域名，并始终使用注册时的域名。

至少注册一个 Passkey 后才能关闭密码登录。关闭前请确认 Passkey 能正常登录，并保留管理员密码。

## 在线更新

进入“关于 → 版本中心”，先点击“检查更新”，确认可更新后再点击“立即更新”。版本发布不代表镜像已构建完成；显示镜像尚未就绪时，稍后重新检查即可。

在线更新需要带更新服务的 Docker 部署，并且部署文件保持官方版本。更新会拉取现成镜像，不会在服务器上编译，也不会主动删除现有账号、配置和流量数据。建议更新前下载一次备份。

按上述 IP 访问步骤修改端口映射后，部署目录存在本地改动，当前在线更新会拒绝执行。更新前请先备份并处理这些改动，避免端口配置被覆盖；不要强行覆盖自己的修改。

如果旧版本没有在线更新功能，需要在保留自定义配置的前提下重新安装，保持安装目录不变。

## 常见问题

| 问题 | 建议处理方式 |
| --- | --- |
| 安装提示缺少 Git、Docker 或 Compose | 先补齐依赖；确认 Docker 已运行，当前用户能执行 `docker info` |
| IP 加端口打不开面板 | 检查服务是否运行、端口映射是否已调整，以及云安全组是否允许访问 TCP `43211` |
| 镜像下载失败或提示 `manifest unknown` | 新版本镜像可能还在构建，稍后重试；下载超时则检查服务器访问 GitHub 和 Docker Hub 的网络 |
| CMS 或 CDT 不可用 | 先用“测试账号”检查权限；“未验证”不是无权限，数据尚未更新时可稍后再试 |
| 无法在线更新 | 确认使用带更新服务的 Docker 部署且没有本地文件修改；镜像未就绪时稍后重试 |
| 提示部署目录存在未提交修改 | 端口映射等自定义配置也属于本地修改；先备份并处理改动，不要强行覆盖 |

## 安全提醒

- 不要公开 AccessKey、AccessKey Secret、初始化 token、备份密码或包含这些信息的日志。
- 建议使用 HTTPS 访问面板；IP 直连时，云安全组尽量只允许可信 IP 访问。
- 只授予需要的子账号权限，不要使用主账号密钥或管理员权限。
- 不要执行 `docker compose down -v` 或随意删除数据卷，否则可能丢失账号、配置和历史数据。

## 请喝咖啡

如果这个工具对你有帮助，欢迎支持项目：

<img src="./static/donation-code.jpg" alt="项目赞赏码" width="240">

- USDT-TRC20：`TMBmXngHcKtNo9nLxTCJcGTfNutsbb9rp3`
- USDT-Polygon / BSC / ERC20 / ArbitrumOne：`0x846ad38ed7159aee70b0ebd8ec39ad2d7b32835b`

## 相关链接

- 项目地址：https://github.com/Kori1c/ecs-controller
- 问题反馈：https://github.com/Kori1c/ecs-controller/issues
- 许可证：[MIT](./LICENSE)
