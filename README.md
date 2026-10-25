# ChinaCPA 🇨🇳 国产大模型反代网关

> 一站式纳管三大国产 AI 编程平台账号，自动签到攒积分、自动刷新额度、自动统计计费，
> 一个 OpenAI 兼容接口随便调 —— **无需下载任何官方客户端**。

| 平台 | 登录方式 | 免费额度 | 每日签到 |
|---|---|---|---|
| **WorkBuddy / CodeBuddy**（腾讯） | OAuth 浏览器授权 | 积分制（资源包） | ✅ 每日签到 + 任务中心自动领奖 |
| **Qwen 通义千问**（chat.qwen.ai） | 设备码授权 | 2000 次请求/天 | —（无签到体系，本地自动计数） |
| **Trae**（字节 SOLO 免费通道，CN/国际） | PKCE 授权码回调 | ide_credits | ✅ 每日 +200 积分 |
| **Qoder**（阿里，CN qoder.com.cn / 国际 qoder.sh） | 控制台 PAT 导入 | credits（套餐+加购+组织） | ✅ 每日积分活动自动领取 |

> Trae 2026-10 起切换到 PKCE 授权码登录（客户端 3.3.62），本项目已跟进新流程；旧版 refreshToken 凭证仍可粘贴导入。
> Qoder 分区：国内版 PAT 直接粘贴；国际版加前缀 `global:`。

## ✨ 功能

- **凭证即用**：面板 OAuth 授权 / PAT / refreshToken 三种纳管方式，不需要安装官方 IDE/CLI
- **四大平台**：WorkBuddy、Qwen、Trae（CN/国际）、Qoder（CN/国际），模型前缀 `workbuddy:` `qwen:` `trae:` `qoder:`
- **自动签到积分**：每日 9 点、21 点自动签到（可配），WorkBuddy 任务中心自动接受任务+领取奖励
- **额度自动获取**：每 10 分钟自动刷新各账号剩余额度（WorkBuddy 积分 / Trae ide_credits / Qwen 今日剩余次数）
- **模型发布 · 渠道归组**：面板自定义对外模型 → 多个上游渠道（跨平台），渠道间轮询 + 失败转移，渠道内多账号再轮询；`/v1/models` 只暴露已发布模型（未配置时默认暴露全部内部模型），配置落盘 `data/models.json`
- **OpenAI 兼容网关**：`/v1/models` + `/v1/chat/completions`（SSE 流式），多账号自动轮询 + 失败转移 + 限流冷却
- **仪表板统计**：今日/7 日 token 消耗（100M/1B 风格）、请求数、按模型费用估算（发布模型单价优先，$/1M → ¥ 自动折算）、剩余额度自动计算
- **单二进制**：纯 Go 标准库，零第三方依赖，下载即用

## 🚀 快速开始

```bash
# 1. 构建（或直接下载 release 的 chinacpa.exe）
go build -o chinacpa.exe ./cmd/chinacpa

# 2. 启动（首次自动生成 config.json）
./chinacpa.exe -config config.json
# Windows 双击 start.bat 也可以

# 3. 打开面板
# http://127.0.0.1:8318/   默认密码 123456
```

### 面板操作

1. 点右上角「添加账号」→ 选平台 → 打开授权链接登录 → 自动纳管 + 自动签到 + 自动拉额度
2. 仪表板实时看：账号池状态、剩余额度、今日用量、费用估算
3. 网关信息区有现成的接入参数（地址 + 密钥）

### 客户端接入（三种原生协议）

网关同时支持三种请求协议（翻译层完整移植自 CLIProxyAPI）：

| 端点 | 协议 | 适用客户端 |
|---|---|---|
| `POST /v1/chat/completions` | OpenAI Chat Completions | OpenAI SDK / 主流龙虾 |
| `POST /v1/responses` | OpenAI Responses API | **Codex CLI**（wire_api=responses） |
| `POST /v1/messages` | Anthropic Messages | **Claude Code**（x-api-key 鉴权） |

**模型别名**：Codex / Claude Code 客户端可能写死模型名，在 config.json 配置映射：

```json
{
  "model_aliases": {
    "gpt-5.2-codex": "workbuddy:glm-5.3",
    "claude-sonnet-4-6": "trae:glm-5.2"
  }
}
```

**模型发布（推荐）**：面板「模型发布」区可视化配置对外模型，一个名字挂多个上游渠道，
渠道自动轮询 + 失败转移，并附带单价（$/1M 输入/输出）用于费用估算。
配置保存在 `data/models.json`，也可手写：

```json
{
  "usd_cny": 7.2,
  "models": [
    {
      "id": "gpt-5",
      "note": "主力",
      "pricing": { "input": 0.002, "output": 0.008 },
      "channels": [
        { "provider": "qwen", "model": "qwen3-coder-plus" },
        { "provider": "workbuddy", "model": "glm-5.3" }
      ]
    }
  ]
}
```

客户端直接 `{"model":"gpt-5"}` 调用即可；配置了发布模型后 `/v1/models` 只返回已发布模型。
各平台原始模型仍可 `平台:模型` 直连（面板「各平台模型 · 计价倍率」区可查全部原始模型与 credits 系数）。

**Codex CLI 接入**（`~/.codex/config.toml`）：

```toml
model = "gpt-5.2-codex"            # 经 model_aliases 映射，或直接写 workbuddy:glm-5.3
model_provider = "chinacpa"
wire_api = "responses"

[model_providers.chinacpa]
name = "ChinaCPA"
base_url = "http://127.0.0.1:8318/v1"
env_key = "CHINACPA_API_KEY"       # export CHINACPA_API_KEY=<你的 api_key>
```

**Claude Code 接入**：

```bash
export ANTHROPIC_BASE_URL=http://127.0.0.1:8318
export ANTHROPIC_API_KEY=<你的 api_key>
export ANTHROPIC_MODEL=trae:glm-5.2        # 或用 model_aliases 映射 claude-sonnet-4-6
```

**标准 OpenAI SDK**：

```bash
curl http://127.0.0.1:8318/v1/chat/completions \
  -H "Authorization: Bearer <你的 api_key>" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "qwen:qwen3-coder-plus",
    "stream": true,
    "messages": [{"role":"user","content":"你好"}]
  }'
```

模型命名：**`平台:模型`**（`workbuddy:deepseek-v4.1` / `qwen:qwen3-coder-plus` / `trae:glm-5.2`）。
裸模型名（如 `glm-5.2`）在唯一平台存在时可直接用；重名时接口会提示可用前缀。

## ⚙️ 配置（config.json）

```jsonc
{
  "listen": ":8318",                    // 监听地址
  "api_key": "sk-xxx",                  // /v1 网关密钥
  "panel_password": "123456",           // 面板密码
  "data_dir": "./data",                 // 账号凭证/用量/状态目录
  "schedule": {
    "checkin_hours": [9, 21],           // 每日自动签到时刻（小时）
    "checkin_enabled": true,
    "tasks_enabled": true,              // WorkBuddy 任务中心自动完成
    "quota_refresh_minutes": 10,        // 额度自动刷新间隔
    "token_refresh_hours": [3]          // token 保活刷新时刻
  },
  "qwen": { "daily_request_limit": 2000 },
  "pricing": {
    "file": "",                         // 自定义单价表（缺省 data/pricing.json）
    "usd_cny": 7.1                      // 费用估算汇率
  }
}
```

### 自定义计价表（可选，`data/pricing.json`）

```json
{
  "usd_cny": 7.1,
  "models": {
    "qwen3-coder-plus": { "input": 0.002, "output": 0.006 },
    "glm-5.2": { "input": 0.002, "output": 0.008 }
  }
}
```

费用估算 = 输入 tokens/1M × input 价 + 输出 tokens/1M × output 价，再按汇率折人民币。
内置常用国产模型参考价，仅供参考，非计费依据。

## 📁 数据目录

```
data/
├── accounts/            # 账号凭证（workbuddy-*.json / qwen-*.json / trae-*.json）
├── usage/               # 每日用量日志（usage-YYYYMMDD.jsonl）
├── state.json           # 额度快照 + 签到历史
├── models.json          # 发布模型配置（对外模型 → 多渠道 + 单价）
└── pricing.json         # 可选自定义单价表
```

## 🔌 HTTP 接口一览

**网关（Bearer api_key）**

| 路由 | 说明 |
|---|---|
| `GET /v1/models` | 模型列表（配置发布模型后只返回已发布模型） |
| `POST /v1/chat/completions` | 对话（流式/非流式） |

**面板（cookie 会话）**

| 路由 | 说明 |
|---|---|
| `POST /api/auth` | 面板登录 |
| `GET /api/overview` | 仪表板数据（账号/额度/今日用量/费用） |
| `GET /api/usage?days=7` | 用量历史 |
| `GET /api/models?refresh=1` | 模型列表 |
| `POST /api/login/start` | 发起 OAuth（`{provider}`） |
| `GET /api/login/poll` | 轮询登录结果 |
| `POST /api/import` | 粘贴凭证导入（`{provider, credential}`） |
| `POST /api/accounts/action` | 账号操作（checkin/quota/refresh_token/toggle） |
| `DELETE /api/accounts` | 删除账号 |
| `POST /api/checkin_all` | 全账号立即签到 |
| `GET/POST /api/publish` | 发布模型配置读写（对外模型 → 多渠道 + 单价） |
| `GET /trae/callback` | Trae 登录回调（浏览器自动跳转） |

## 🧩 架构

```
cmd/chinacpa            入口
internal/
├── model/              公共类型（账号/额度/模型/登录会话）
├── config/             配置加载
├── store/              凭证/状态/签到历史落盘（原子写）
├── provider/           平台抽象 + 三实现
│   ├── workbuddy/      腾讯：OAuth、积分、签到、任务中心、模型、对话
│   ├── qwen/           通义：设备码 OAuth、2000次/天计数、模型、对话直通
│   └── trae/           字节：授权回调、ExchangeToken、签到、额度、SOLO SSE→OpenAI
├── pool/               多账号池：轮询、冷却、模型路由（发布模型多渠道 / 前缀 / 裸名）
├── pubmodel/           发布模型配置（对外模型 → 多渠道 + 单价）
├── scheduler/          定时：签到、任务、额度刷新、保活
├── usage/              用量 JSONL + 聚合 + 计价估算（发布单价优先）
├── gateway/            OpenAI 兼容网关（SSE 透传、流内 usage 提取）
└── panel/              管理 API + 内嵌仪表板（embed 单文件 HTML）
```

## ⚠️ 注意

- 本项目仅供学习研究，请遵守各平台服务条款；上游风控收紧时对应通道可能失效
- 各平台凭证即金钱：`data/` 目录请妥善保管，不要外传
- WorkBuddy 刷新 token 有防重放（12153），本工具已做单账号串行刷新；多进程同时跑同一 data 目录有风险
- Trae 登录回调要求面板与浏览器同机（127.0.0.1 回调）或局域网可达；服务器部署时可用「粘贴凭证」方式纳管

## 🙏 参考

接口口径来自以下开源项目的实测成果（均已 fork 到 [XingQiPan](https://github.com/XingQiPan?tab=repositories)）：

- [linguo2625469/workbuddy2api-panel](https://github.com/linguo2625469/workbuddy2api-panel)（WorkBuddy 全套）
- [connectedGraph/trae2api-web](https://github.com/connectedGraph/trae2api-web)（Trae SOLO 通道）
- [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI)（Qwen OAuth，v6.0.0 实现）
- [ardeyouxipianyi/workbuddy2api-hub](https://github.com/ardeyouxipianyi/workbuddy2api-hub)（计价/用量思路）

## License

MIT
