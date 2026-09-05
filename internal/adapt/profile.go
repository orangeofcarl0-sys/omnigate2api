// Package adapt 反代协议适配层：上游能力描述（Profile）与决策组件。
//
// 设计原则（见 docs/SPEC-adaptation-layer.md）：
//   - 上游差异全部声明化：接入新上游 = 写一份 Profile，不改内核；
//   - 默认面 = 原生等价（模型所见 = 客户端所发）；增量是显式 opt-in；
//   - 任何语义偏差可观测、可熔断、可回退。
package adapt

import (
	"fmt"
	"strings"
)

// UpstreamProfile 一份上游能力描述（七组字段）。字段缺省值见 §4.2 校验规则。
type UpstreamProfile struct {
	ID      string `yaml:"id"`
	Display string `yaml:"display"`

	Inbound   InboundProfile   `yaml:"inbound"`
	Message   MessageProfile   `yaml:"message"`
	Stream    StreamProfile    `yaml:"stream"`
	Session   SessionProfile   `yaml:"session"`
	Auth      AuthProfile      `yaml:"auth"`
	Limits    LimitsProfile    `yaml:"limits"`
	Tool      ToolProfile      `yaml:"tool"`
	Toolchain ToolchainProfile `yaml:"toolchain"`
}

// InboundProfile 入站协议面（SPEC §13.4）：启用的入站端点；空 = 三者全开。
type InboundProfile struct {
	Protocols []string `yaml:"protocols"`
}

// Allows 判断某协议是否启用。
func (p *InboundProfile) Allows(proto string) bool {
	if len(p.Protocols) == 0 {
		return true
	}
	for _, x := range p.Protocols {
		if x == proto {
			return true
		}
	}
	return false
}

// MessageProfile 消息形态：roles（结构化）或 text-only（需折叠）。
type MessageProfile struct {
	Model string `yaml:"model"`
	// 非文本块语义（SPEC §30.2）：placeholder=占位折叠（缺省，§13.3）/
	// passthrough=图片分片透传（仅限 roles，Validate fail-fast）；env 覆盖 OMNIGATE_MEDIA
	Media   string         `yaml:"media,omitempty"`
	Folding *FoldingConfig `yaml:"folding,omitempty"`
}

// MediaMode 有效 media 模式：缺省 placeholder（原生等价默认：未声明行为零变化）。
func (m *MessageProfile) MediaMode() string {
	if m != nil && m.Media == "passthrough" {
		return "passthrough"
	}
	return "placeholder"
}

// FoldingConfig 折叠渲染配置：角色标记与收尾护栏。
type FoldingConfig struct {
	Markers    map[string]string `yaml:"markers"`
	GuardBlock string            `yaml:"guard_block"`
	// 非文本块的占位文本模板（§13.3），含 {type} 占位符；空 = 内置缺省
	MediaPlaceholder string `yaml:"media_placeholder,omitempty"`
}

// StreamProfile 流式帧语义：增量/快照事件、终止信号、EOF 与合成策略。
type StreamProfile struct {
	DeltaEvents      []string `yaml:"delta_events"`
	SnapshotEvents   []string `yaml:"snapshot_events"`
	Terminators      []string `yaml:"terminators"`
	EOFIsTerminal    bool     `yaml:"eof_is_terminal"`
	SynthesizeFinish bool     `yaml:"synthesize_finish"`
}

// SessionProfile 会话语义：none（无状态）/ explicit（显式 ID）/ implicit（隐式记忆）。
type SessionProfile struct {
	Kind  string `yaml:"kind"`
	Trust string `yaml:"trust,omitempty"` // implicit 时：low|medium|high
	// ReanchorEvery 重锚定间隔（轮）；0 = 按 trust 默认（low=5/medium=10/high=20）。
	ReanchorEvery int `yaml:"reanchor_every,omitempty"`
}

// AuthProfile 鉴权与续期能力。
type AuthProfile struct {
	RefreshSupported bool   `yaml:"refresh_supported"`
	ReauthCommand    string `yaml:"reauth_command,omitempty"` // 如 relogin.bat 的提示
	// ClientFamily 客户端家族（SPEC §24.1）：账号池按家族隔离与分发客户端；
	// 缺省 "codearts"（华为签名 / Bearer 腾讯为 "workbuddy"）。
	ClientFamily string `yaml:"client_family,omitempty"`
}

// Family 有效客户端家族（缺省 codearts）。
func (p *AuthProfile) Family() string {
	if p.ClientFamily != "" {
		return p.ClientFamily
	}
	return "codearts"
}

// LimitsProfile 限流/错误分类能力。
type LimitsProfile struct {
	RateLimitHints      []string `yaml:"rate_limit_hints"`
	SoftCooldownSeconds int      `yaml:"soft_cooldown_seconds"`
	ErrCooldownSeconds  int      `yaml:"err_cooldown_seconds"`
	ErrThreshold        int      `yaml:"err_threshold"`
}

// ToolProfile 工具调用模拟层配置。
type ToolProfile struct {
	FenceOpen         string `yaml:"fence_open"`
	BracketCallPrefix string `yaml:"bracket_call_prefix"`
	JSONRepair        bool   `yaml:"json_repair"`
	PostSuppress      bool   `yaml:"post_suppress"`
	FenceTolerantScan bool   `yaml:"fence_tolerant_scan"`
}

// ToolchainProfile 工具层（SPEC §14）：出站前可选 stage 链；指针存在即启用。
type ToolchainProfile struct {
	Project  *ProjectConfig  `yaml:"project,omitempty"`  // 有损投影（默认关）
	Sanitize *SanitizeConfig `yaml:"sanitize,omitempty"` // 反监控（默认关）
}

// ProjectConfig 有损投影参数（SPEC §14.2）。
type ProjectConfig struct {
	AgenticDetect bool `yaml:"agentic_detect"` // 命中 agentic 特征记建议日志（缺省 true）
	HistoryItems  int  `yaml:"history_items"`  // 摘要保留条数（默认 10）
	HistoryChars  int  `yaml:"history_chars"`  // 摘要总字符上限（默认 2200）
	TailMessages  int  `yaml:"tail_messages"`  // 尾保留消息数（默认 8）
	TailChars     int  `yaml:"tail_chars"`     // 尾字符上限（默认 7000）
	AnchorUser    bool `yaml:"anchor_user"`    // 保留最早投影掉的 user（默认 true）
	ToolMaxChars  int  `yaml:"tool_max_chars"` // 工具 arguments/output 上限（默认 1600）
}

// SanitizeConfig 反监控参数（SPEC §14.3）。
type SanitizeConfig struct {
	Mode          []string              `yaml:"mode"` // zwsp|strip|compact，至少一项
	Terms         []string              `yaml:"terms,omitempty"`
	StripMetadata bool                  `yaml:"strip_metadata"`
	HarnessUser   bool                  `yaml:"harness_user"`
	HarnessBlocks []HarnessBlock        `yaml:"harness_blocks,omitempty"`
	Fingerprints  []SanitizeFingerprint `yaml:"fingerprints,omitempty"`
}

// HarnessBlock 开闭标记块 → 中性替身（SPEC §14.3 mode=strip）。
type HarnessBlock struct {
	Start   string `yaml:"start"`
	End     string `yaml:"end"`
	Replace string `yaml:"replace"`
}

// SanitizeFingerprint 键值/header 型指纹剥离或逐字改写（SPEC §14.3）。
type SanitizeFingerprint struct {
	Match   string   `yaml:"match"`
	Rewrite []string `yaml:"rewrite,omitempty"` // [原文, 改文]；空则视为整段剥离标记
}

// Defaults：未显式配置的字段采用的内置缺省。
var Defaults = struct {
	TrustReanchor map[string]int // trust → 重锚定轮数
}{
	TrustReanchor: map[string]int{"low": 5, "medium": 10, "high": 20},
}

// Codearts 内置 Profile：华为 MaaS 实证为标准 OpenAI 兼容多模态端点（§31.1：
// image_url 分片接受 / 原生 tools 不可用 / 对网关无状态），整通道 roles 透传
// （SPEC §31.2 根治拍板）：真实历史数组直传 + 图片分片；工具走围栏模拟
// （注入 roles 末条 user，输出转录渠道无关）。
var Codearts = UpstreamProfile{
	ID:      "codearts",
	Display: "Huawei Cloud CodeArts Agent",
	Inbound: InboundProfile{
		// 三协议全部实现（§13.4 缺省=全开）；需收窄时由 YAML 覆盖
	},
	Message: MessageProfile{
		Model: "roles",
		Media: "passthrough", // §31.2：图片分片原生直传（MaaS 实证接受）
	},
	Stream: StreamProfile{
		DeltaEvents:      []string{"", "message", "delta", "content", "onanswer", "answer"},
		Terminators:      []string{"done", "end", "finish"},
		EOFIsTerminal:    true,
		SynthesizeFinish: true,
	},
	Session: SessionProfile{
		Kind: "none", // §31.2：上游无状态实证（chat_id 不上线），每轮全量数组
	},
	Auth: AuthProfile{
		RefreshSupported: false, // 华为 OAuth 授权码流程不返回 refresh_token
		ReauthCommand:    "relogin.bat",
	},
	Limits: LimitsProfile{
		RateLimitHints:      []string{"429", "rate limit", "MaaS"},
		SoftCooldownSeconds: 45,
		ErrCooldownSeconds:  600,
		ErrThreshold:        3,
	},
	Tool: ToolProfile{
		FenceOpen:         "```tool_call",
		BracketCallPrefix: "[助手调用工具 ",
		JSONRepair:        true,
		PostSuppress:      true,
		FenceTolerantScan: true,
	},
}

// Workbuddy 内置 Profile：腾讯 WorkBuddy/CodeBuddy（copilot.tencent.com，
// SPEC §25）。roles 原生透传 + 无会话（指纹增量待实测）；客户端家族
// workbuddy（Bearer）；sanitize 默认开是该上游的实证例外（腾讯内容审核
// 误伤合规模板，参考实现验证为刚需）。
var Workbuddy = UpstreamProfile{
	ID:      "workbuddy",
	Display: "Tencent WorkBuddy/CodeBuddy (copilot.tencent.com)",
	Message: MessageProfile{
		Model: "roles",
		// SPEC §30.9 实证（2026-09-06）：官方客户端源码证实 chat 图片 = OpenAI
		// image_url 分片 + data URI 内联（user/tool 同机制），切 passthrough 落地
		Media: "passthrough",
	},
	Stream: StreamProfile{
		DeltaEvents:      []string{"", "message", "delta", "content"},
		Terminators:      []string{"done", "end", "finish"},
		EOFIsTerminal:    true,
		SynthesizeFinish: true,
	},
	Session: SessionProfile{
		Kind: "none", // §21.2：默认全量；实测有会话记忆后再评估 implicit
	},
	Auth: AuthProfile{
		RefreshSupported: true,
		ClientFamily:     "workbuddy",
		ReauthCommand:    "login-tencent",
	},
	Limits: LimitsProfile{
		RateLimitHints:      []string{"429", "rate limit"},
		SoftCooldownSeconds: 45,
		ErrCooldownSeconds:  600,
		ErrThreshold:        3,
	},
	Toolchain: ToolchainProfile{
		Sanitize: &SanitizeConfig{Mode: []string{"zwsp", "strip", "compact"}},
	},
}

// Validate 校验 Profile 完整性（fail-fast；见 SPEC §4.2）。
func (p *UpstreamProfile) Validate() error {
	if p.ID == "" {
		return fmt.Errorf("profile: id required")
	}
	if p.Message.Model == "text-only" && p.Message.Folding == nil {
		return fmt.Errorf("profile %q: text-only requires folding config", p.ID)
	}
	// media 语义校验（SPEC §30.2）：passthrough 仅限 roles（text-only 无像素通道）
	if p.Message.Media != "" && p.Message.Media != "placeholder" && p.Message.Media != "passthrough" {
		return fmt.Errorf("profile %q: invalid message.media %q", p.ID, p.Message.Media)
	}
	if p.Message.Media == "passthrough" && p.Message.Model != "roles" {
		return fmt.Errorf("profile %q: media passthrough requires message.model=roles", p.ID)
	}
	if p.Session.Kind == "implicit" && p.Session.Trust == "" {
		return fmt.Errorf("profile %q: implicit session requires trust", p.ID)
	}
	if p.Session.Kind != "none" && p.Session.Kind != "explicit" && p.Session.Kind != "implicit" {
		return fmt.Errorf("profile %q: invalid session kind %q", p.ID, p.Session.Kind)
	}
	if p.Session.Kind == "implicit" {
		if _, ok := Defaults.TrustReanchor[p.Session.Trust]; !ok {
			return fmt.Errorf("profile %q: invalid trust %q", p.ID, p.Session.Trust)
		}
	}
	for _, proto := range p.Inbound.Protocols {
		if proto != "chat" && proto != "anthropic" && proto != "responses" {
			return fmt.Errorf("profile %q: invalid inbound protocol %q", p.ID, proto)
		}
	}
	if s := p.Toolchain.Sanitize; s != nil && len(s.Mode) == 0 {
		return fmt.Errorf("profile %q: sanitize.mode required", p.ID)
	}
	if len(p.Limits.RateLimitHints) == 0 {
		return fmt.Errorf("profile %q: rate_limit_hints required", p.ID)
	}
	return nil
}

// ReanchorEvery 有效重锚定间隔（轮）：显式值优先，否则按 trust 缺省。
func (p *UpstreamProfile) ReanchorEvery() int {
	if p.Session.ReanchorEvery > 0 {
		return p.Session.ReanchorEvery
	}
	if n, ok := Defaults.TrustReanchor[p.Session.Trust]; ok {
		return n
	}
	return 5
}

// IsRateLimit 判断错误文本是否属于限流（LimitsProfile 驱动，替代硬编码串匹配）。
func (p *UpstreamProfile) IsRateLimit(msg string) bool {
	lower := strings.ToLower(msg)
	for _, hint := range p.Limits.RateLimitHints {
		if strings.Contains(lower, strings.ToLower(hint)) {
			return true
		}
	}
	return false
}
