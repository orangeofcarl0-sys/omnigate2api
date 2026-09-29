# CodeArts Agent 逆向记录（2026-08-03，版本 26.7.0）

> 素材：`D:\CodeArts Agent\resources\app`（VS Code 系，未打包目录），
> 扩展 `vscode-codebot`（agent + snap API 客户端）、`huaweicloud.authentication`
> （登录）、`huaweicloud.codearts-agent-support`，以及 `product.json` 域名表。

## 1. 架构

CodeArts Agent 是 VS Code 系 Electron 应用（vscode 1.109.5 定制）。AI 对话走
**华为云 snap-access 盘古引擎**：

- 聊天：`POST https://snap-access.cn-north-4.myhuaweicloud.com/v1/chat/chat`（SSE）
- 登录：华为云 CodeArts OAuth2（PKCE），`snap-manager/v1/oauth2/tokens` 换 STS 临时凭证
- 鉴权：聊天用 `x-auth-token: <security_token>`；部分账号 API 用临时 AK/SK 签名

客户端里还带一个 Cline 系 agent 内核（`agentkernelServer-*.exe`，Bun 打包的本地
HTTP 服务），负责本地 agent 循环（会话/工具/文件），云端对话仍是上面的 chat 接口。

## 2. 关键端点（商业版 cn-north-4）

| 用途 | 方法/路径 | 鉴权 |
| --- | --- | --- |
| 聊天 | POST /v1/chat/chat | x-auth-token: security_token |
| 开始聊天 | POST /v1/chat/start-chat | 同上 |
| Agent 列表 | GET /v1/chat/agents | 同上 |
| 反馈事件 | POST /v1/chat/management/event | 同上 |
| 记录请求 | POST /v1/chat/record-request | 同上 |
| 换 token | POST /v1/oauth2/tokens（authorization_code / refresh_token） | client_id + code/refresh_token |
| ticket 轮询 | GET /v1/login/ticket?ticket_id=&secret= | plugin-name/version 头 |
| 当前用户 | GET /v1/current/user | AK/SK 签名 + X-Security-Token |
| 账号信息 | GET /v5/caller-identity（sts.cn-north-4） | AK/SK 签名 + X-Security-Token |

域名表（product.json commercialVersionDomain.newFramework.productDomain）：
`chatDomain = codeGenDomain = snapEngineDomain = snap-access.cn-north-4.myhuaweicloud.com`；
`toolkitDomain = iam.myhuaweicloud.com`；`iamStsOpenDomain = sts.cn-north-4.myhuaweicloud.com`。

## 3. 登录流程（huaweicloud.authentication）

1. 生成 `ticket_id`（32 hex）、`secret`（32 hex）、PKCE `code_verifier/code_challenge`。
2. 本地监听 `127.0.0.1:{port}/oauth/callback`。
3. 构造 `https://codearts.huaweicloud.com/authorize?client_id=...&port=...&code_challenge=...&code_challenge_method=S256&ticket_id=...&plugin-name=huaweicloud.authentication&plugin-version=...`。
4. 双通道拿结果：
   - 回调带 `code` → `POST snap-manager/v1/oauth2/tokens`
     `{client_id, code, code_verifier, grant_type:"authorization_code", redirect_uri:"http://127.0.0.1:{port}/oauth/callback"}`
   - 轮询 `GET snap-manager/v1/login/ticket?ticket_id=&secret=`（插件头）
5. 响应含 `{user_id, user_name, domain_id, refresh_token, credentials:{access_key_id, secret_access_key, security_token, expiration}}`。
6. 刷新：`POST https://sts.cn-north-4.myhuaweicloud.com/v1/oauth2/tokens`
   `{client_id, code_verifier, grant_type:"refresh_token", refresh_token}`（同样带 DPoP）。

`CLIENT_ID` = 应用 `uri_scheme`（默认 `codearts`，实测以登录链接为准，可在 login 工具
用 `-client-id` 覆盖）。

## 4. 聊天请求

Header：
```
Content-Type: application/json
Accept: text/event-stream
X-Sdk-Date: <YYYYMMDDTHHMMSSZ>
X-Security-Token: <STS security_token>（临时凭证时）
X-Sdk-Content-Sha256: <payload sha256>
Authorization: SDK-HMAC-SHA256 Access=<AK>, SignedHeaders=..., Signature=...
x-snap-traceid: <随机>
```

**鉴权不是 x-auth-token**：实测 x-auth-token 传 STS security_token 会被 APIG 拒
（APIG.0301 decrypt token fail）。桌面端走 `GlobalCredentialClient`，即华为云
AK/SK `SDK-HMAC-SHA256` 签名（signed headers = 请求全部头，CanonicalURI 带尾斜杠，
payload hash 取 X-Sdk-Content-Sha256）。已按该算法实现并实测通过。

Body：
```json
{"chat_id":"<32位hex>","client":"IDE","task":"chat",
 "messages":[{"type":"text","text":"..."}],
 "task_parameters":{"ide":"CodeArts Agent"},
 "batch_task_parameters":[],"attempt":1,"not_allow_external_model":true,
 "user_id":"<用户名>"}
```

实测要点：
- `chat_id` 必须是 **32 位十六进制**（UUID 去连字符），否则报「请求参数错误：chat_id」
- `messages` 是 **内容块数组（无 role）** `[{type:"text",text}]`，
  不是 OpenAI 的 {role,content}，否则报「请求参数错误：messages」
- 携带 `user_id`（用户名）更稳妥

## 5. SSE 格式

实测为**逐行 `data:` JSON（无空行分隔，部分行也无 event 前缀）**：
- 起始：`{"id":...,"model":"glm-4.7","type":"answer","chat_id":...,"response_message_id":...}`
- 全文快照：`{"text":"<当前完整文本>","output":[],"prompt_tokens":...,"completion_tokens":...}`
  （text 是**累计全文**，非增量，解析时用替换语义）
- 增量：`{"delta":{"content":"...","reasoning_content":"..."}}`（is_delta_response 场景）
- 结束：`{"text":"[DONE]","error_code":"0",...}`；错误形如
  `{"text":"[DONE]","error_code":"ChatAgent.00001001","error_msg":"..."}`
- `output` 数组：终态 `[{type:"output_text",text}]`

## 6. 额度/签到核对结论

- **没有每日签到接口**。
- **没有「申请额度」按钮**（免费额度按月重置，见 codearts 个人用量页）。
- 因此本项目用「token 自动 refresh 续期」作为对应自动签到的能力，
  并保留 `credit.sh`/`apply.sh` 运维脚本。

## 7. 脱敏

本仓库是**公开**仓库，所有记录（含本文档的端点/错误码/字段名实证）都会被检索到。脱敏口径如下。

### 7.1 敏感面：账号标识与凭证（必须处理）

- 本仓库**不包含任何真实凭证**：`auths/`、`data/`、`config.json`、`.env`、`*.key`、`*.pem`
  一律 gitignore；`HANDOFF.md`（本地交接文档，含运维上下文与账号清单）同样 gitignore。
- 写文档/提交信息时，账号一律用占位符：uid → `<uid>` / `<account-A>`，昵称与邮箱不落盘。
- **探测输出必须写进仓库根的点前缀文件**（`.xxx.txt` / `.xxx.json`）或 `/tmp`：根目录点文件
  已**整体忽略**（`.gitignore` 里 `/.*` + 三个白名单 `.gitignore`/`.dockerignore`/`.env.example`）。
  之所以改成按前缀整体忽略：历史上 `.ts2.txt`、`.ex_full.txt` 这类探测导出被误提交并推送，
  内容里带着真实 uid；逐个往 .gitignore 补名字永远追不上新名字。

### 7.2 提交前守卫

`tools/hooks/pre-commit` 扫描**暂存新增行**与**提交信息**，命中即拦下并列出具体行号：

| 拦截项 | 形态 |
|---|---|
| 账号 uid | `uid=<20+ 位>`、32 位十六进制、UUID |
| 个人邮箱 | gmail/qq/163/126/outlook/hotmail/foxmail |
| 本机路径 | 系统用户目录（Windows 的 Users 路径、类 Unix 的 home 目录） |
| 凭证字段被赋实值 | `refresh_token`/`access_key_id`/`secret_access_key`/… 带值 |
| JWT / 私钥 | `eyJ…`、`PRIVATE KEY` 块 |

启用（幂等，一次即可）：`tools/install-git-hooks.sh`（等价于 `git config core.hooksPath tools/hooks`）。
确需提交时用 `git commit --no-verify` 绕过。守卫自身文件已从扫描中排除（其模式字面量会命中自己）。

### 7.3 边界：上游协议细节不作为敏感信息

端点路径、请求头（含官方 CLI/桌面 UA）、业务码（6004/11128/14018/12153…）、字段名
（`modelPromotions`、`CapacityRemain`…）**必须存在于代码中**否则无法工作，只脱敏文档属安全
表演；且生态内同类公开项目（见 README「参考项目」）已记录同样的端点与错误码。因此本仓库
对这类内容不作屏蔽，仅对**账号标识、凭证、本机路径**脱敏。

### 7.4 历史重写记录（2026-09-24）

排查发现有 uid 通过两个误提交的探测文件（`.ts2.txt`、`.ex_full.txt`，均已从工作树删除）留在
**历史对象**里——工作树干净 ≠ 没泄漏。已用 `git filter-repo` 重写全部历史（`--invert-paths`
移除这两个文件 + `--replace-text` 兜底替换），并强推。

- 结果：全历史与对象库（含不可达对象）中账号标识 **0 命中**，`git fsck` 无损坏；
- 代价：**所有提交 SHA 改变**（远端 `main` 由 `a5fcab3` → `e9d94d9`），标签内容未受影响；
- 其它机器上的克隆需重新克隆或 `git fetch --force && git reset --hard origin/main`；
- 动手前的镜像备份留在仓库**外部**（`../omnigate2api-backup-<ts>.git`），需要彻底清除时删除它；
- 注意 GitHub 侧的对象缓存/已 fork 副本可能短期保留旧数据，这一层本地无法控制。

## 附：登录续期排查（2026-09-24）

**问题**：华为 STS 令牌寿命约 24h，凭证无 refresh_token → 无法自动续期，账号每日左右需重登。

**排查结论（两轮真实登录实证）**：

| 通道 | 端点 | 实测结果 |
|---|---|---|
| ticket 轮询 | `GET {snap-manager}/v1/login/ticket?ticket_id&secret` | ✅ 登录成功；**响应不含 refresh_token**（落盘凭证 `refresh_token` 长度 0；代码侧已确认 `TokenResponse.RefreshToken` 会被 `saveLoginResult` 写入 → 是服务端未下发） |
| code 回调 | 门户 → `http://127.0.0.1:{port}/oauth/callback?code=…` → `POST {sts}/v1/oauth2/tokens`（authorization_code） | ❌ **未触发**：门户只回第一阶段回调（`code_bytes=0 secret_bytes=64 redirect_len=278`），网关 307 把浏览器送回门户后，门户**不再回带 code**（浏览器显示「登录失败」）；两轮均如此 |

**第三轮实证（2026-09-24，协议回传假设 → 已证伪）**：注册 `HKCU\Software\Classes\codearts`
协议处理器（tools/install-codearts-uri.ps1）后重跑登录：

- 门户**从未触发** `codearts://`（处理器日志无调用记录）；
- 首次回调日志显示 `redirect_to=codearts.huaweicloud.com/portal/login`——门户把浏览器
  **送回自己的登录页**，即它不认为该浏览器会话处于已登录态；
- 结论：**code 通道在普通系统浏览器流程下不可达**。官方客户端能走通，推测因其本身即
  插件的 Webview 上下文（共享插件身份/Cookie/UA），而外部浏览器缺少该上下文。
  处理器已注销（不留无用的 scheme 劫持面）；如需再试可重跑安装脚本。

**原始推断（保留备查）**：authorize URL 携带 `uri_scheme=codearts`，门户的授权确认可能依赖**自定义协议回传**（官方客户端注册了 `codearts://`，纯浏览器无该处理器故停在失败页）。若成立，可行的根治路径是：在 Windows 注册 `codearts://` 协议 → 指向本地小工具 → 由它拿 code 走 `authorization_code` 换取（该响应按老记录含 `refresh_token`）→ 接入现有 `RefreshToken` 自动续期。

**已落地的可诊断性改进**：`OMNIGATE_LOGIN_DEBUG=1`（compose 透传）打印 ticket 响应是否含 refresh_token；首次回调日志记录 `redirect_to={host}{path}`（去查询串，防 secret 泄露），用于判断门户把浏览器引向何处。

## 附二：登录续期结案（2026-09-27，推翻上节两条推断）

**素材升级**：本机装有官方客户端 `~/.codeartsdoer/CodeArts_Agent/`（26.8.203），其
`AgentKernel_Vscode_26_8_203_1_0_0-*.exe` 是 Bun 打包但**内嵌 JS 源码可读**，
直接读出了认证模块（`src/custom-hw/**/auth`）——比 2026-08-03 只读 `resources/app` 更硬。

**推翻 ①「code 通道依赖 Webview 上下文/自定义协议」**：真正的原因是**门户会话 Cookie**。
2026-09-24 三轮失败的当口，浏览器并没有 codearts.huaweicloud.com 的登录态（日志自证
`redirect_to=…/portal/login`）。2026-09-27 复测（浏览器已有登录态）：回调仍在第一阶段
`secret_bytes=64 redirect_to=…/portal/login`，但**没有人做任何操作**，约 15 秒后 ticket 轮询
就成功了。即：浏览器这一步是必须的（第一阶段下发 `secret` 要靠它），但**只要门户 Cookie
在，这一步就不需要人**。判定实验：同一天另起会话**不打开浏览器** → 30s 内 0 次回调、
poll 恒 pending。

**更正 ②（同日第二轮实测，**推翻上面这段结论**）**："门户从不回带 code"是**错的**——不是门户
不发，是**我们 authorize URL 的两个参数发错了**。四次登录对照（每次只改一处）：

1. 现状 `code_challenge_method=S256` + `client_id=codearts` → `code_bytes=0 secret_bytes=64`
2. 只加 `auth_callback_url=…/oauth/callback` → 同上（单独加不够）
3. **`code_challenge_method=SHA-256` + `auth_callback_url`**（其余不动）→ **`code_bytes=32`** ✅
4. 社区 26.9.x 全形态（`client_id=uri_scheme=vscode-codebot` + `SHA-256` + `auth_callback_url`
   + `plugin-version=26.9.101`）→ `code_bytes=32` ✅

与"官方常量 `PKCEGenerator.CODE_CHALLENGE_METHOD = "SHA-256"`，错值回退旧 ticket 流程"
（社区多项目注释，称对齐真实插件）完全吻合。官方源码 `parseTicketResponse()` 仍硬编码
`return { refreshToken: "", expiresAt: data.credential.expires_at, credentials: {...} }`
——**ticket 通道确实不发 refresh_token**；refresh_token 只在 code 通道的 `parseTokenResponse()`
里读。**长期方案 = 授权码 → refresh_token**（绑定 client_id + DPoP 私钥 + `code_verifier`，
一次性轮换，详见 SPEC §24.5）。

**其余源码事实**（对齐全用得上）：`CLIENT_ID = "CodeArts_Tui"`；authorize URL 只带
`locale/client_id/port/code_challenge/code_challenge_method="SHA-256"/ticket_id`；
`OAUTH_CALLBACK_PATH = "/oauth/callback"`（与我们一致）；`CALLBACK_TIMEOUT_MS = 5min`；
`TOKEN_EXPIRY_BUFFER_MS = 10min`（我们 30min 更保守）；`refreshToken()` 需要
`client_id/refresh_token/grant_type=refresh_token` + `DPoP` + `x-agent-user-account`；
`ensureValidToken()` 可用 `CODEARTS_CLI_AK/SK` 短路（CLI 模式）——但实测该 AK/SK 形态直接
打 `snap-access` chat 会被拒（**两次实测**：头完全缺失；以及头存在但为空 + 正确签名 →
均 `APIG.0301 decrypt token fail`）。该网关**强制要求临时凭证的 security_token**；
官方 CLI 文档的"永久 AK/SK"要走 IAM `POST /v3.0/OS-CREDENTIAL/securitytokens` 自换
15min–24h 临时凭证（此路未实测，需控制台建密钥）。

**结论**：长期方案 = **OAuth 授权码 → `refresh_token` 静默续期**（authorize URL 用
`code_challenge_method=SHA-256` + `auth_callback_url`；凭证落盘 refresh_token + DPoP 私钥 +
`code_verifier`，刷新后回写轮换 token、同账号加锁）。兜底 = 零人工周期性重登
（`tools/huawei-relogin.sh --auto` + 每 6h 定时任务）。细节与可行性矩阵见
`docs/SPEC-adaptation-layer.md` §24.5 与 `HANDOFF.md` §6.5.2。
