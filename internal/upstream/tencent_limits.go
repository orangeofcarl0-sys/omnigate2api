// 腾讯模型级限流（429 + code 6004）的识别与解封时刻解析（SPEC §28.4 决策 B 补充）。
//
// 实测形态（社区多份生产样本一致，2026-09）：
//
//	HTTP 429 {"code":6004,"msg":"您的使用量已超出频率限制，将在 2026-09-22 09:46:39 UTC+8
//	          重置，您也可以切换其他模型继续使用。","requestId":"..."}
//	国际版英文："Your usage has exceeded the rate limit. It will reset at 2026-09-05 01:57:00 UTC+8."
//
// 关键语义：**上游自证这是模型级限制**（"您也可以切换其他模型继续使用"）——同账号其它
// 模型照常可用。因此正确处置是「把 (账号, 模型) 这对冷却到上游声明的解封时刻，并换号重试」，
// 而不是把整账号软冷却（那会平白丢掉该账号对其余模型的容量）。
//
// 另注：网上流传的"每模型 2 亿 token 硬上限"未找到任何一手证据；上游 6004 的 msg 只给
// 解封时刻、不给累计 token 数，故按"额度耗尽"实现会误伤——此处只认上游自述的模型级限流。
package upstream

import (
	"regexp"
	"strconv"
	"strings"
	"time"
)

// isModelRateLimitMarkers 模型级限流文案标记（中英）。
var isModelRateLimitMarkers = []string{
	"使用量已超出频率限制", "usage has exceeded the rate limit", "usage exceeds frequency limit",
	"切换其他模型", "switch to another model",
}

// modelRateLimitCode 上游模型级限流的业务码。
const modelRateLimitCode = 6004

// IsModelRateLimit 判定响应是否属于**模型级**限流：业务码 6004（首选）或文案标记。
// 不带任何证据的裸 429 不按模型级处理（无法区分账号级/渠道级，交给通用软冷却兜底）——
// 宁可整账号短暂冷却，也不要在没证据时把"账号级限流"误当成"只限这个模型"继续打。
func IsModelRateLimit(body string) bool {
	if c, ok := TencentBizCode(body); ok && c == modelRateLimitCode {
		return true
	}
	low := strings.ToLower(body)
	for _, m := range isModelRateLimitMarkers {
		if strings.Contains(low, strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// TencentBizCode 从响应体取业务码（腾讯统一 envelope 的 code 字段）。
func TencentBizCode(body string) (int64, bool) {
	re := regexp.MustCompile(`"code"\s*:\s*(-?\d+)`)
	m := re.FindStringSubmatch(body)
	if len(m) != 2 {
		return 0, false
	}
	n, err := strconv.ParseInt(m[1], 10, 64)
	if err != nil {
		return 0, false
	}
	return n, true
}

// tencentResetRes 解封时刻文案（中英两式，时间戳为 UTC+8）。
// 注意语序：中英都是「时间在前、重置动词在后」（"将在 <t> 重置" / "reset at <t>"），
// 只给时刻不给日期的形态同样在前，别按动词在前写。
var tencentResetRes = []*regexp.Regexp{
	regexp.MustCompile(`(?:将在|将于)\s*(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`),
	regexp.MustCompile(`reset at\s*(\d{4}-\d{2}-\d{2}[ T]\d{2}:\d{2}:\d{2})`),
	regexp.MustCompile(`(?:将在|将于)\s*(\d{2}:\d{2}:\d{2})`),
	regexp.MustCompile(`reset at\s*(\d{2}:\d{2}:\d{2})`),
	regexp.MustCompile(`(?:将在|将于)\s*(\d{2}:\d{2})`),
}

// ParseTencentResetAt 解析上游声明的解封时刻（缺日期时按"今天该时刻，若已过则明天"补全）。
// 返回零值 + false 表示没解析出来（调用方退化到默认窗口）。
func ParseTencentResetAt(body string, now time.Time) (time.Time, bool) {
	local := now.In(cstZone)
	for _, re := range tencentResetRes {
		m := re.FindStringSubmatch(body)
		if len(m) != 2 {
			continue
		}
		s := m[1]
		if len(s) == 8 { // 只有 HH:MM:SS
			t, err := time.Parse("15:04:05", s)
			if err != nil {
				continue
			}
			at := time.Date(local.Year(), local.Month(), local.Day(), t.Hour(), t.Minute(), t.Second(), 0, cstZone)
			if !at.After(local) {
				at = at.Add(24 * time.Hour)
			}
			return at, true
		}
		for _, layout := range []string{"2006-01-02 15:04:05", "2006-01-02T15:04:05"} {
			if t, err := time.ParseInLocation(layout, s, cstZone); err == nil {
				return t, true
			}
		}
	}
	return time.Time{}, false
}

// ModelRateLimitUntil 计算 (账号, 模型) 对的冷却截止时刻：优先用上游声明的解封时刻，
// 解析失败则退回 fallback；上限钳在 maxHorizon 内（防上游给出离谱时间把账号废掉）。
func ModelRateLimitUntil(body string, now time.Time, fallback, maxHorizon time.Duration) time.Time {
	until := now.Add(fallback)
	if t, ok := ParseTencentResetAt(body, now); ok && t.After(now) {
		until = t
	}
	if maxHorizon > 0 && until.After(now.Add(maxHorizon)) {
		until = now.Add(maxHorizon)
	}
	return until
}
