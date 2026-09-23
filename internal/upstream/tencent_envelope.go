// 腾讯业务信封（SPEC §32 统一解包）：四个业务域共用一套 code/msg 判定，
// 消除各处重复的错误构造（"一个机制一条路径"）。局部仍各自 inline 解包
// （各端点 data 形状差异大，共用泛型解包收益低于可读性损失）。
package upstream

import "fmt"

// bizEnvelope 业务信封（腾讯统一响应形态：code≠0 为业务错误，msg 为说明）。
type bizEnvelope struct {
	Code int64  `json:"code"`
	Msg  string `json:"msg"`
}

// checkBiz 业务双门槛判定（HTTP 状态 + 业务码）：非 nil 即为错误。
// 传字段值而非信封类型：各端点的 inline 解包结构带各自 data 字段，类型不互通。
// label 沿用各域既有措辞（日志可读性优先），格式统一由 bizErr 保证。
func checkBiz(label string, status int, code int64, msg string) error {
	if status >= 400 || code != 0 {
		return bizErr(label, status, code, msg)
	}
	return nil
}

// bizErr 统一业务错误构造：label + HTTP 状态 + 业务码 + 截断消息。
func bizErr(label string, status int, code int64, msg string) error {
	return fmt.Errorf("%s failed http=%d code=%d msg=%s", label, status, code, truncateStr(msg, 200))
}
