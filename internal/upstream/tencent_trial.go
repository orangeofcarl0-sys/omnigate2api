// 国际版账号试用自动激活（SPEC §28.6）。
//
// 为什么要有这个：新加的国际版账号即便能登录、能签权，任何模型都回
// `429 code=14017 The trial version is not yet activated`，余额接口 500 —— 因为该账号在站点侧
// 还没走完开通流程。上游文案让人"退出登录再登录"，实测重登无效；真因是**站点侧的开通/激活
// 步骤没做**（人工做法是在网页上填一次国家/地区）。与其让人去点网页，不如照官方前端的做法自动跑。
//
// 形状来源（2026-09-29 从官方**登录前端** bundle 提取，原文在
// `download.codebuddy.ai/web/login/<hash>/assets/billing-<hash>.js`，该 chunk 全文只有这两条路径）：
//
//	a = async () => n.post("/billing/ide/trial")                      // 幂等：14051 = 已领
//	w = async e => {                                                  // registerCloud
//	  const t = await n.get("/auth/realms/copilot/overseas/user/register", {params: {userId: e}});
//	  if (t?.code !== 200 && t?.code !== 0) throw new Error(t?.msg || "registerCloud failed");
//	}
//	o = async (uid, uin, type) => {
//	  if (!uin && (!type || type !== PERSONAL)) { await w(uid); await sleep(1500) }   // 先注册、等 1.5s
//	  return true
//	}
//	S = uid => o(uid).then(ok => ok && a())                            // 登录后自动跑（不 await）
//
// 即 **register → 等 1.5s → trial**。实测（已激活账号上两者都幂等）：
//
//	GET /auth/realms/copilot/overseas/user/register?userId=<uid> → {"code":200,"msg":"register success"}
//	POST /billing/ide/trial                                      → {"code":14051,"msg":"has applied trial"}
//
// 地区未设置时上游会要求补地区：官方前端由用户在 RegisterRegion 页选国家，然后
// `POST /console/login/account {attributes:{countryCode:[..],countryFullName:[..],countryName:[..]}}`。
// 那条分支的可实证样本我们还没拿到（`/billing/area/get-country-code` 要求特定的 url/User-Agent，
// 我们暂时取不到国家表），所以这里用**可配置的默认国家**（`OMNIGATE_TRIAL_COUNTRY`，
// 默认 `SG:Singapore:新加坡`，与我方全球账号实证一致），并且**只在**上游明确报"地区类"错误时才写，
// 原始响应一律进日志，等真遇到时按日志对齐取值。
package upstream

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"omnigate2api/internal/auth"
)

// trialRegisterGrace register 与 trial 之间的等待：官方前端写死 1500ms（上游侧似有生效延迟），
// 这里原样保留——少等可能白跑一次 trial。变量形式仅为让测试把它调成 0。
const trialRegisterGrace = 1500 * time.Millisecond

// trialGrace 实际使用的等待（测试可覆盖）。
var trialGrace = trialRegisterGrace

// trialPathRegister / trialPathTrial 两个端点都在站点源（global → www.workbuddy.ai）。
const (
	trialPathRegister = "/auth/realms/copilot/overseas/user/register"
	trialPathTrial    = "/billing/ide/trial"
	trialPathRegion   = "/console/login/account"
)

// TrialActivation 一次试用激活的结果（面板/日志用；字段都是可观测事实）。
type TrialActivation struct {
	Skipped      bool   `json:"skipped,omitempty"`      // 非国际版账号
	RegisterCode int64  `json:"register_code"`          // 0/200 = 成功
	RegisterMsg  string `json:"register_msg,omitempty"` //
	TrialCode    int64  `json:"trial_code"`             // 0/200 = 开通成功；14051 = 早就领过
	TrialMsg     string `json:"trial_msg,omitempty"`    //
	AlreadyHad   bool   `json:"already_had,omitempty"`  // trial=14051
	RegionSet    bool   `json:"region_set,omitempty"`   // 本次补写过地区
	Note         string `json:"note,omitempty"`         // 人类可读结论
	Raw          string `json:"raw,omitempty"`          // 出错时的原始响应（截断）
}

// OK 报告试用已可用（新开通或早就领过）。
func (t *TrialActivation) OK() bool {
	return t != nil && (t.TrialCode == 0 || t.TrialCode == 200 || t.AlreadyHad)
}

// EnsureGlobalTrial 为国际版账号补全试用激活（register → 1.5s → trial）。
// 幂等：已激活的账号会拿到 register success + 14051，可安全地在每次登录后调用。
func (c *TencentClient) EnsureGlobalTrial(acct *auth.Auth) (*TrialActivation, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for trial activation")
	}
	out := &TrialActivation{}
	if !TencentRegion(acct.Domain) {
		out.Skipped = true
		out.Note = "非国际版账号，跳过试用激活"
		return out, nil
	}

	code, msg, raw, err := c.registerCloud(acct)
	out.RegisterCode, out.RegisterMsg = code, msg

	// 上游要求补地区：**先判语义再判错**——"需补地区"本身就是以错误码回来的
	// （实测形态：http=200 code=500 msg="region required" 之类），若先当硬失败返回就永远补不上。
	// 补法是官方前端那条：写国家属性（三个值取自国家表，我们用 OMNIGATE_TRIAL_COUNTRY 默认新加坡）。
	if err != nil && trialNeedsRegion(msg) {
		if rerr := c.setTrialRegion(acct); rerr != nil {
			out.Raw = truncateStr(raw, 300)
			return out, fmt.Errorf("register needs region and setting it failed: %w", rerr)
		}
		out.RegionSet = true
		time.Sleep(trialGrace)
		code, msg, raw, err = c.registerCloud(acct)
		out.RegisterCode, out.RegisterMsg = code, msg
	}
	if err != nil {
		out.Raw = truncateStr(raw, 300)
		return out, fmt.Errorf("register failed: %w", err)
	}

	// 官方前端在这里 sleep(1500) 再领试用。
	time.Sleep(trialGrace)

	tcode, tmsg, traw, terr := c.claimTrial(acct)
	out.TrialCode, out.TrialMsg = tcode, tmsg
	out.AlreadyHad = tcode == trialAlreadyAppliedCode
	switch {
	case terr != nil:
		// 地区类错误在 trial 阶段才暴露：补地区后重试一次。
		if trialNeedsRegion(tmsg) {
			if rerr := c.setTrialRegion(acct); rerr == nil {
				out.RegionSet = true
				time.Sleep(trialGrace)
				tcode, tmsg, traw, terr = c.claimTrial(acct)
				out.TrialCode, out.TrialMsg = tcode, tmsg
				out.AlreadyHad = tcode == trialAlreadyAppliedCode
			}
		}
		if terr != nil {
			out.Raw = truncateStr(traw, 300)
			out.Note = "试用激活失败"
			return out, fmt.Errorf("trial claim failed: %w", terr)
		}
	case trialNeedsRegion(tmsg):
		if rerr := c.setTrialRegion(acct); rerr == nil {
			out.RegionSet = true
			time.Sleep(trialGrace)
			tcode, tmsg, traw, terr = c.claimTrial(acct)
			out.TrialCode, out.TrialMsg = tcode, tmsg
			out.AlreadyHad = tcode == trialAlreadyAppliedCode
			if terr != nil {
				out.Raw = truncateStr(traw, 300)
				return out, fmt.Errorf("trial claim failed after region: %w", terr)
			}
		}
	}

	switch {
	case out.AlreadyHad:
		out.Note = "试用此前已开通（14051）"
	case out.TrialCode == 0 || out.TrialCode == 200:
		out.Note = "试用已自动开通"
	default:
		out.Raw = truncateStr(traw, 300)
		out.Note = fmt.Sprintf("试用返回未识别状态 code=%d msg=%s", out.TrialCode, truncateStr(tmsg, 120))
		return out, fmt.Errorf("trial claim unexpected code=%d msg=%s", out.TrialCode, truncateStr(tmsg, 160))
	}
	return out, nil
}

// trialAlreadyAppliedCode 试用已领（幂等成功，不是错误）。
const trialAlreadyAppliedCode = 14051

// registerCloud GET {billing}/auth/realms/copilot/overseas/user/register?userId=<uid>。
// 返回 (业务 code, msg, 原始响应, error)：code ∈ {0,200} 视为成功。
func (c *TencentClient) registerCloud(acct *auth.Auth) (int64, string, string, error) {
	raw, status, err := c.billingDo(acct, "GET", trialPathRegister+"?userId="+acct.UserID, nil)
	if err != nil {
		return 0, "", "", err
	}
	code, msg := tencentEnvelopeCodeMsg(raw)
	if status >= 400 || (code != 0 && code != 200) {
		return code, msg, string(raw), fmt.Errorf("%s http=%d code=%d msg=%s",
			trialPathRegister, status, code, truncateStr(msg, 160))
	}
	return code, msg, string(raw), nil
}

// claimTrial POST {billing}/billing/ide/trial。
// 返回 (业务 code, msg, 原始响应, error)：14051（已领）**不算错误**，由调用方判定。
func (c *TencentClient) claimTrial(acct *auth.Auth) (int64, string, string, error) {
	raw, status, err := c.billingDo(acct, "POST", trialPathTrial, []byte("{}"))
	if err != nil {
		return 0, "", "", err
	}
	code, msg := tencentEnvelopeCodeMsg(raw)
	if status >= 400 {
		return code, msg, string(raw), fmt.Errorf("%s http=%d code=%d msg=%s",
			trialPathTrial, status, code, truncateStr(msg, 160))
	}
	return code, msg, string(raw), nil
}

// setTrialRegion 补写国家/地区：官方前端形状是
// `{attributes:{countryCode:[..], countryFullName:[..], countryName:[..]}}`——
// 三个值都取自国家表（Code/EnName/IOS2），取值由 OMNIGATE_TRIAL_COUNTRY 配置
// （默认 `SG:Singapore:新加坡`，与我方全球账号实证一致）。
func (c *TencentClient) setTrialRegion(acct *auth.Auth) error {
	cc, full, name := trialCountry()
	body, _ := json.Marshal(map[string]any{
		"attributes": map[string]any{
			"countryCode":     []string{cc},
			"countryFullName": []string{full},
			"countryName":     []string{name},
		},
	})
	raw, status, err := c.billingDo(acct, "POST", trialPathRegion, body)
	if err != nil {
		return err
	}
	code, msg := tencentEnvelopeCodeMsg(raw)
	if status >= 400 || (code != 0 && code != 200) {
		return fmt.Errorf("%s http=%d code=%d msg=%s", trialPathRegion, status, code, truncateStr(msg, 160))
	}
	log.Printf("trial region set account=%s country=%s/%s/%s", acct.UserID, cc, full, name)
	return nil
}

// trialCountry 试用激活时补写的国家：OMNIGATE_TRIAL_COUNTRY="<code>:<EnName>:<本地名>"。
func trialCountry() (code, full, local string) {
	v := strings.TrimSpace(os.Getenv("OMNIGATE_TRIAL_COUNTRY"))
	if v != "" {
		parts := strings.Split(v, ":")
		for len(parts) < 3 {
			parts = append(parts, "")
		}
		if parts[0] != "" {
			return parts[0], parts[1], parts[2]
		}
	}
	return "SG", "Singapore", "新加坡"
}

// trialNeedsRegion 判定上游是否在要求补国家/地区（中英文案都收）。
// 我们还没有该分支的可实证样本，所以判据取宽（宁可多写一次地区，也不要把新号卡住），
// 且命中时把原始响应打进日志，等真实样本回来再收紧。
func trialNeedsRegion(msg string) bool {
	l := strings.ToLower(msg)
	for _, m := range []string{"region", "country", "area required", "地区", "国家", "region required"} {
		if strings.Contains(l, m) {
			return true
		}
	}
	return false
}

// tencentEnvelopeCodeMsg 从 {code,msg,...} 信封里取业务码与消息（非信封返回 0/空）。
func tencentEnvelopeCodeMsg(raw []byte) (int64, string) {
	var env struct {
		Code int64  `json:"code"`
		Msg  string `json:"msg"`
	}
	if json.Unmarshal(raw, &env) != nil {
		return 0, ""
	}
	return env.Code, env.Msg
}
