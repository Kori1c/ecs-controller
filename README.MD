# ECS 控制台

一个简单的阿里云 ECS 管理面板，可以查看实例状态和流量，并在流量达到阈值时自动关机，避免产生额外费用。

> 阿里云流量数据可能存在延迟，自动关机不能保证绝对不会产生费用。

## 部署

准备一台安装了 Docker 的公网 Linux 服务器。如果使用域名，请先将域名解析到服务器 IP。

在服务器上运行：

```bash
curl -fsSL https://raw.githubusercontent.com/Kori1c/ecs-controller/main/install.sh \
  | env ECS_COOKIE_SECURE=1 bash
```

安装脚本会自动下载镜像并启动服务，不会在服务器上编译镜像。服务默认只监听服务器本机的 `43211` 端口，请通过你现有的 HTTPS 访问方式打开面板，不要直接开放这个端口。

公网使用 Passkey 时必须使用 HTTPS。

### 本地测试

在自己的电脑上测试时，可以使用 Docker Desktop 或 Colima，安装后访问 `http://127.0.0.1:43211`。

## 第一次使用

1. 安装完成后，脚本会显示一个初始化 token。
2. 打开网页，输入 token 并设置管理员密码，密码至少 6 位。
3. 进入“账号管理”，添加阿里云账号。
4. 在“系统设置”中设置流量阈值、自动关机和通知方式。

如果忘记了初始化 token，可以查看日志：

```bash
docker logs ecs-controller 2>&1 | grep '初始化 token'
```

## 阿里云权限

建议创建一个专门给本项目使用的 RAM 用户（阿里云子账号），不要使用主账号密钥，也不需要授予管理员权限。

### 怎么添加权限

1. 打开阿里云控制台，搜索并进入“访问控制 RAM”。
2. 创建用户，开启“OpenAPI 调用访问”，创建并保存 AccessKey ID 和 AccessKey Secret。
3. 给这个用户“添加权限”，选择“系统策略”，按下面的名称搜索并添加。
4. 将该用户的 AccessKey ID 和 AccessKey Secret 填入本面板的“账号管理”。

### 管理已有实例和监控流量

添加下面 3 项。权限名称可以直接复制搜索，点击名称可查看阿里云官方说明。

| 权限名称 | 用途 |
| --- | --- |
| [AliyunECSFullAccess](https://help.aliyun.com/zh/ram/developer-reference/aliyunecsfullaccess) | 查看和管理 ECS，支持开关机、流量超限自动关机和定时开关机 |
| [AliyunCloudMonitorReadOnlyAccess](https://help.aliyun.com/zh/ram/developer-reference/aliyuncloudmonitorreadonlyaccess) | 读取每台实例的流量（面板中的 CMS），以及弹性公网 IP（EIP）的地址和带宽 |
| [AliyunCDTReadOnlyAccess](https://help.aliyun.com/zh/ram/developer-reference/aliyuncdtreadonlyaccess) | 读取账号总流量（面板中的 CDT） |

注意：这是方便配置的方案，不是最小权限方案。`AliyunECSFullAccess` 也允许创建和删除 ECS；只授予 ECS 只读权限则无法自动关机。

### 按需添加的权限

| 想使用的功能 | 额外添加的权限 |
| --- | --- |
| 在面板中创建实例，并自动创建网络 | [AliyunVPCFullAccess](https://help.aliyun.com/zh/ram/developer-reference/aliyunvpcfullaccess) |
| 创建实例时使用 EIP、更换 EIP，或释放实例时清理 EIP | [AliyunEIPFullAccess](https://help.aliyun.com/zh/ram/developer-reference/aliyuneipfullaccess) |
| 查看余额和消费账单 | [AliyunBSSReadOnlyAccess](https://help.aliyun.com/zh/ram/developer-reference/aliyunbssreadonlyaccess) |

只管理已有实例、不更换或释放 EIP、不查看账单时，不需要添加上面 3 项。**VPC 权限不包含 EIP 管理权限**，使用 EIP 管理功能时需要单独添加 EIP 权限。创建实例和 EIP 可能产生费用，请按需授权。

如果提示“CMS 不可用”或“CDT 不可用”，先检查对应的只读权限是否已添加；如果提示“云端数据尚未更新”，可稍后再试。账号测试通过不代表所有功能的权限都已齐全。

## 主要功能

- 查看 ECS 状态、规格、IP 和流量。
- 查看 CMS 实例流量和 CDT 账号流量。
- CMS 或 CDT 任一流量达到阈值时自动关机。
- 定时开关机和抢占式实例自动恢复。
- 创建、启动、停止和释放 ECS。
- Telegram、邮件和 Webhook 通知。
- Passkey 免密码登录。
- 自定义控制台名称。
- 在系统设置中创建加密备份，并恢复账号、设置、历史数据和 Passkey。

### 数据备份

进入“系统设置 → 全局设置”，输入备份密码后下载备份文件。恢复时选择备份文件并输入同一个密码。

备份密码不会保存在系统中，请单独妥善保存。恢复会覆盖当前账号和设置，完成后需要使用备份中的管理员凭据重新登录。

## 在线更新

进入“关于 → 版本中心”，点击检查更新即可。

只有 GitHub 上的 Docker 镜像已经准备好时才会更新，部署机器不会构建镜像。更新不会删除账号、配置和流量数据。

如果旧版本没有在线更新功能，重新运行一次安装命令即可。

## 安全提醒

- 不要把 AccessKey、AccessKey Secret 或初始化 token 发给别人。
- 公网访问请使用 HTTPS，不要直接暴露 `43211` 端口。
- 建议使用 RAM 子账号，不要直接使用阿里云主账号密钥。

## Passkey

登录后进入“系统设置 → 全局设置”，点击“设置 Passkey”。以后可以使用指纹、面容、系统 PIN 或安全密钥登录。

公网使用 Passkey 时必须配置 HTTPS；本地测试可以使用 `http://localhost`。

## 请喝咖啡

如果这个工具对你有帮助，欢迎支持项目：

![项目赞赏码](./static/donation-code.jpg)

- USDT-TRC20：`TMBmXngHcKtNo9nLxTCJcGTfNutsbb9rp3`
- USDT-Polygon / BSC / ERC20 / ArbitrumOne：`0x846ad38ed7159aee70b0ebd8ec39ad2d7b32835b`

## 相关链接

- 项目地址：https://github.com/Kori1c/ecs-controller
- 问题反馈：https://github.com/Kori1c/ecs-controller/issues
- 许可证：[MIT](./LICENSE)
