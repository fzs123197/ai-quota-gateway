# AI Quota Gateway

单用户自用的多协议 LLM 额度代理网关。

入口同时支持 **OpenAI Chat Completions / OpenAI Responses / Anthropic Messages**；上游协议按模型配置，三种协议之间自动互转。网关用 Redis 保存多 Key、多额度方案、credits 三重限额（月 / 周 / 5 小时滑动窗口）和用量统计，自带额度面板与管理台。

仓库：https://github.com/fzs123197/ai-quota-gateway

## 功能

- 三协议入口：
  - `POST /v1/chat/completions`
  - `POST /v1/responses`
  - `POST /v1/messages`
  - `POST /v1/messages/count_tokens`
  - `GET /v1/models`
- 上游协议由 `upstream[].protocol` 指定：`chat`（默认）/ `responses` / `anthropic`。
- 同协议原样透传；跨协议自动转换请求、响应和 SSE 流（文本、图片、function tools、tool_result、usage 等）。
- 统一的 **credits** 计费：token 限额和金额限额合并为一套额度，按模型价格自动计算消耗倍率。
- 5 小时滑动窗口基于 Redis ZSet，窗口连续滑动，不是固定时间桶。
- 多 Key / 多 Plan：每个 Key 绑定一个方案，切换方案立即生效。
- 管理台：密钥、额度方案、用量统计、模型消耗倍率。
- Windows 一键启动：自动拉起便携版 Redis 和网关。

## 快速开始（Windows）

### 1. 克隆

```bat
git clone https://github.com/fzs123197/ai-quota-gateway.git
cd ai-quota-gateway
```

### 2. 准备配置

复制示例配置并填写密钥：

```bat
copy config.example.json config.json
```

需要修改：

- `gateway.api_key`：客户端访问网关使用的 Key
- `gateway.admin_key`：管理台 Key
- `upstream[].api_key`：上游厂商的真实 API Key
- （可选）`upstream[].price`：手动定价，单位与 `pricing.currency` 一致
- （可选）`upstream[].protocol`：`chat` / `responses` / `anthropic`

第一次运行 `start-local.bat` 时，如果 `config.json` 不存在，脚本会自动从 `config.example.json` 复制一份，并提示你编辑。

### 3. 启动

双击 `start-local.bat`，或在当前目录执行：

```bat
start-local.bat
```

脚本会：

1. 检查/构建 `ai-quota-gateway.exe`
2. 启动 `tools\redis\redis-server.exe`（默认 `127.0.0.1:6379`，数据保存在 `data\redis`）
3. 启动网关（默认 `127.0.0.1:8080`）
4. 退出时停止本次脚本启动的 Redis

启动后：

- 额度面板：`http://127.0.0.1:8080/?key=你的 gateway.api_key`
- 管理台：`http://127.0.0.1:8080/admin?key=你的 gateway.admin_key`
- OpenAI 兼容 Base URL：`http://127.0.0.1:8080/v1`

> 仓库已经包含 Windows x64 的 `ai-quota-gateway.exe` 和便携 Redis，不需要预装 Go 或 Redis。
> 如果 6379 端口已有 Redis，脚本会直接复用，不会停止它。
> 如果强制关闭窗口导致 Redis 残留，可手动执行：
> `tools\redis\redis-cli.exe -p 6379 shutdown save`

## 配置说明

```json
{
  "gateway": {
    "mode": "local",
    "api_key": "gw-CHANGE-ME",
    "admin_key": "gw-admin-CHANGE-ME",
    "listen_addr": "127.0.0.1:8080"
  },
  "redis": {
    "addr": "127.0.0.1:6379",
    "db": 0
  },
  "quota": {
    "month": 10000000,
    "week": 3000000,
    "five_hour_window": 300000
  },
  "pricing": {
    "enabled": true,
    "currency": "CNY"
  },
  "upstream": [
    {
      "model_name": "deepseek-chat",
      "protocol": "chat",
      "provider": "deepseek",
      "base_url": "https://api.deepseek.com/v1",
      "api_key": "sk-...",
      "price": { "input": 2, "output": 8, "cache_read": 0.5 }
    }
  ]
}
```

- `gateway.mode`：
  - `local`：只监听回环地址，适合本机自用
  - `service`：按 `listen_addr` 监听，适合服务器 + systemd
- `quota.*`：**credits**，不是 token，也不是金额。
- `pricing.enabled` 必须为 `true`，否则网关无法按价格计算 credits。
- `upstream[].protocol`：`chat` / `responses` / `anthropic`。
- `upstream[].price`：手动价格，单位是 `pricing.currency`；优先级最高。目录价格统一按美元/1M tokens 处理。
- 未定价的模型会被拒绝（400）；请配置 `price`、`price_model` 或 `pricing.fallback`。
- `config.example.json` 中 DeepSeek / GLM 的手动价格是示例估算值，请按你的实际账单调整；Qwen 使用价格目录。

## Credits 与额度方案

**1 credit = 0.000001 USD。**

模型倍率（credits/token）由价格自动计算：

- 倍率 = 模型美元单价 / 1M tokens
- 一次请求消耗的 credits = 实际美元成本 × 1,000,000
- 缓存命中按 cache_read 单价计算
- 因此 token 限额与金额限额天然对齐：方案的 credits 花完，实际花费就是 `credits × 0.000001 USD`
- 管理台「用量统计 → 模型消耗倍率」会展示每个已定价模型的输入/输出/缓存倍率

内置预设（月 / 周 / 5 小时，单位 credits）：

| 预设 | 月 | 周 | 5 小时 | 满月约 |
|---|---:|---:|---:|---:|
| Go | 2,000,000 | 600,000 | 60,000 | $2 |
| Plus | 5,000,000 | 1,500,000 | 150,000 | $5 |
| Pro | 10,000,000 | 3,000,000 | 300,000 | $10 |
| Max 10x | 100,000,000 | 30,000,000 | 3,000,000 | $100 |
| Max 20x | 200,000,000 | 60,000,000 | 6,000,000 | $200 |

- 周额度是月额度的 30%，5 小时额度是周额度的 10%。
- Max 10x / Max 20x 分别是 Pro 的 10x / 20x。
- 满月金额会按实时汇率换算成 `pricing.currency`（例如 CNY）。
- 预设会写入 Redis，可在管理台编辑；内置预设不可删除。
- 默认方案使用 `config.json` 的 `quota.*`（示例为 `10,000,000 / 3,000,000 / 300,000`）。

## 客户端配置

### OpenAI Chat Completions

- Base URL：`http://127.0.0.1:8080/v1`
- API Key：`gateway.api_key`
- 模型：`/v1/models` 返回的模型名

### OpenAI Responses

- Base URL：`http://127.0.0.1:8080/v1`
- 调用 `POST /v1/responses`
- 注意：网关是无状态的，不支持 `previous_response_id`，请发送完整上下文。

### Anthropic Messages

- Base URL：`http://127.0.0.1:8080`
- 调用 `POST /v1/messages`
- 鉴权：`x-api-key: gateway.api_key`（也支持 Bearer）
- `POST /v1/messages/count_tokens` 使用本地估算

## 管理台

访问：

```text
http://127.0.0.1:8080/admin?key=你的 admin_key
```

- **密钥**：新建、停用、删除 Key，切换方案
- **额度方案**：从 Go/Plus/Pro/Max 预设创建或编辑方案，查看满月约金额
- **用量统计**：请求数、Token、花费、按模型/按密钥汇总、模型消耗倍率

## 从源码构建

需要 Go 1.22+：

```bat
go build -o ai-quota-gateway.exe ./src
go test ./...
```

开发运行：

```bat
go run ./src
```

运行时请确保 `config.json`、`index.html`、`admin.html` 与可执行文件在同一目录。

## Linux / macOS

一键脚本和内置 Redis 只针对 Windows。其他系统：

```bash
# 安装 Redis 5+，推荐 Redis 7
redis-server --daemonize yes

go build -o ai-quota-gateway ./src
cp config.example.json config.json
# 编辑 config.json
./ai-quota-gateway
```

仓库内的 `ai-gateway.service` 是 systemd 示例；部署时把 exe、`config.json`、`index.html`、`admin.html` 放到 `/opt/ai-gateway`，并按实际用户/路径调整。

## 目录结构

```text
.
├── src/                     Go 源码与测试
├── tools/redis/             便携版 Redis 5（Windows x64）
├── index.html               用户额度面板
├── admin.html               管理台
├── config.example.json      示例配置（可提交）
├── config.json              本地配置（已 gitignore）
├── start-local.bat          Windows 一键启动入口
├── start.ps1                启动/停止 Redis 与网关
├── ai-quota-gateway.exe     Windows x64 可执行文件
├── ai-gateway.service       systemd 示例
├── go.mod / go.sum
├── README.md
└── THIRD_PARTY_NOTICES.md
```

## 从旧版本升级

- `quota.cost` 已移除；现在 `quota.month / week / five_hour_window` 直接表示 credits。
- 内置方案（Go/Plus/Pro/Max 10x/Max 20x）和默认方案会在启动时一次性迁移到 credits 规则；迁移后在管理台的修改会保留。
- 旧的 token/金额额度计数不再使用，credits 用量从 0 开始；历史用量统计仍保留。
- 自定义方案的数值不变，但语义从 token 变成 credits，请按需调整。
- 未定价模型现在会返回 400（价格尚未加载时返回 503），请配置价格或 `pricing.fallback`。

## 已知限制

- Responses 的 `previous_response_id` / 服务端会话存储不支持。
- 只转换 function tools；`web_search` 等内置工具不转换。
- 跨协议不支持 `n > 1`。
- `response_format`、Anthropic thinking signature、`top_k` 等厂商特有字段跨协议时不转换。
- 跨协议流式转 Anthropic 时，`message_start.input_tokens` 是本地估算值。
- tool_result 内的图片目前只保留文本。
- 未定价模型会被拒绝；如果价格源暂时未加载，会返回 503，请稍后重试。

## 安全

- 请修改示例中的 `gateway.api_key` / `gateway.admin_key` 和所有上游 API Key。
- `config.json` 已被 `.gitignore` 忽略，切勿提交真实密钥。
- `local` 模式只监听回环地址；需要对外提供服务时使用 `service` 模式，并放在 HTTPS 反向代理之后。
- 便携 Redis 默认只绑定 `127.0.0.1` 且无密码；不要把它暴露到公网。

## 第三方组件

Windows 便携 Redis 来自：

- Redis 5.0.14.1：https://github.com/redis/redis/tree/5.0
- Windows 构建：https://github.com/tporadowski/redis

Redis 使用 BSD-3-Clause 许可证，完整文本见 `tools/redis/LICENSE.txt`，其他说明见 `THIRD_PARTY_NOTICES.md`。

## License

本仓库的应用代码未指定开源许可证。第三方 Redis 二进制遵循其各自的 BSD-3-Clause 许可证。
