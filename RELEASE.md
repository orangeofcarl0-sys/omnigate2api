# omnigate2api — 本地增强版 v1.0.0

fork 自 HITZY2002/omnigate2api,基于华为云 CodeArts Agent(盘古助手/码道)
OpenAI 兼容代理的本地强化版。私有仓维护,未回馈上游。

## 发布内容

1. 登录链路修复: portal 二段握手(secret+redirect→code 下发)
2. 活动福利模型: maas_type 头 + 每日自动领取 + 网关 config/balance 封装
3. 真流式工具调用: 增量解析 + 围栏容错 + post 抑制 + 叙述即意图
4. 会话指纹续接(路线 D): 前缀哈希链 → tail 增量折叠,提示词 O(增量)
5. 稳定性: 取消豁免 / 429 软冷却 / finish_reason 补发 / UTF-8 边界修复
6. 观测: chat fold 日志 + TRANSCRIPT_ECHO 检测落盘 + agentdump 调试工具
7. 架构: chatCompletions 复杂度 63→41,测试覆盖围栏/指纹/限流/回声场景

## 部署

docker compose up -d --build(先 cp config.example.json config.json 并设置
OMNIGATE_API_KEY;auths/ 放凭证;activity 模型需账号有当日福利额度)

## 已验证(2026-08-30)

- pi / dsh 双客户端: 16 轮长会话零回声零限流,增量折叠曲线恒定 ~3.6k
- 活动模型三只实测可用;纯流式/非流式/工具流式回归全绿
