// 会话指纹索引：利用「同一 ZCode 会话每轮消息数组是上一轮前缀 + 增量」的
// 特性，以滚动哈希链识别续接，从而把全量折叠降为增量折叠（tail 模式）——
// 大幅降低每轮提示词体积（漂移面与 MaaS 限流压力同比缩小）。
//
// 语义：
//   - H_i = sha256(H_{i-1} ‖ role ‖ content ‖ toolcalls)；
//   - 每个成功回合注册 H_{n-1}（n 为该轮消息数）；
//   - 新请求 n' > n 且 H_{n-1} 一致 → 前缀命中，增量为 messages[n:]。
package server

// 会话语义层（session）：指纹表、指令锚点、contended 串线锁、重锚定（SPEC §5）。

import (
	"crypto/sha256"
	"encoding/hex"
	"sync"
)

const (
	fpMaxEntries = 128
	// 连续增量续接上限：上下文管理归客户端（agent），反代不做裁剪；
	// 增量语义等价性依赖上游会话记忆，无法验证，故以较密的全量重锚定
	// 限损——失忆影响至多最近 fpMaxTails 轮，且重锚定总是以客户端
	// 完整 messages 为准。
	fpMaxTails = 5
)

// sessionRef 会话索引条目。
type sessionRef struct {
	account   string
	chatID    string
	msgsLen   int  // 注册时的消息数（前缀长度）
	tails     int  // 已连续增量的次数
	contended bool // 该 key 曾被不同会话占用(身份翻转)：续接一律拒绝
}

// sessionIndex 指纹表（mutex + 简单 LRU）。
type sessionIndex struct {
	mu            sync.Mutex
	entries       map[string]*sessionRef
	order         []string // 插入顺序，淘汰最旧
	reanchorEvery int      // 连续增量上限（Profile.trust 驱动，SPEC §5.2）
}

func newSessionIndex(reanchorEvery int) *sessionIndex {
	if reanchorEvery <= 0 {
		reanchorEvery = 5
	}
	return &sessionIndex{entries: map[string]*sessionRef{}, reanchorEvery: reanchorEvery}
}

// fingerprint 计算消息数组的滚动哈希链：out[i] = H 前 i+1 条消息。
// 图片以 canonical URL 参与（SPEC §30.6）：同图重发 → 前缀命中可续接，改图 → 回退全量。
func fingerprint(msgs []openAIMessage) []string {
	out := make([]string, len(msgs))
	var h [32]byte
	var buf []byte
	for i, m := range msgs {
		buf = buf[:0]
		buf = append(buf, h[:]...)
		buf = append(buf, m.Role...)
		buf = append(buf, 0)
		buf = append(buf, m.Text...)
		buf = append(buf, 0)
		for _, im := range m.Images {
			buf = append(buf, "img"...)
			buf = append(buf, im.URL...)
			buf = append(buf, 0)
			buf = append(buf, im.Detail...)
			buf = append(buf, 0)
		}
		for _, c := range m.ToolCalls {
			buf = append(buf, c.ID...)
			buf = append(buf, c.Name...)
			buf = append(buf, c.Arguments...)
		}
		h = sha256.Sum256(buf)
		out[i] = hex.EncodeToString(h[:])
	}
	return out
}

// MatchRefusal 续接拒绝原因（供守卫计数，SPEC §5.3）。
type MatchRefusal int

const (
	MatchOK        MatchRefusal = iota
	MatchNoEntry                // 无匹配（普通全量路径）
	MatchAmbiguous              // 同长度多候选（歧义）
	MatchContended              // 会话身份翻转
	MatchReanchor               // 超过重锚定上限
)

// match 查找最深的严格前缀匹配（chain 由调用方预先计算）。
// 命中时返回（ref, 增量起点, 匹配条目键）。仅接受 msgsLen < n 的条目
// （同长度重复请求视为重试，不续接）。
func (s *sessionIndex) match(msgs []openAIMessage, chain []string) (*sessionRef, int, string, MatchRefusal, bool) {
	n := len(msgs)
	s.mu.Lock()
	defer s.mu.Unlock()
	best := -1
	var bestRef *sessionRef
	bestKey := ""
	ambiguous := false
	for key, ref := range s.entries {
		if ref.msgsLen >= n || ref.msgsLen <= 0 {
			continue
		}
		if chain[ref.msgsLen-1] != key {
			continue
		}
		switch {
		case ref.msgsLen > best:
			best = ref.msgsLen
			bestRef = ref
			bestKey = key
			ambiguous = false
		case ref.msgsLen == best:
			// 同长度多个候选（跨会话同前缀，如重开同任务/子代理并存）：
			// map 迭代无序，任取一个会把本会话路由到别人的 chatID——
			// 上下文串线。歧义即回退全量新建，宁可多折叠一次，绝不猜。
			ambiguous = true
		}
	}
	refuse := MatchNoEntry
	if ambiguous {
		refuse = MatchAmbiguous
	}
	if bestRef != nil && bestRef.contended {
		refuse = MatchContended
	}
	if bestRef != nil && bestRef.tails >= s.reanchorEvery {
		refuse = MatchReanchor
	}
	if bestRef == nil || ambiguous || bestRef.contended || bestRef.tails >= s.reanchorEvery {
		return nil, 0, "", refuse, false
	}
	return bestRef, best, bestKey, MatchOK, true
}

// register 注册/更新一个成功回合（key 为消息数组的最终哈希）。
// 同一上游会话（chatID）的延续：继承增量计数并在新键下替换旧条目。
func (s *sessionIndex) register(key string, msgsLen int, account, chatID string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.entries) >= fpMaxEntries {
		for i, k := range s.order {
			if _, ok := s.entries[k]; ok {
				delete(s.entries, k)
				s.order = append(s.order[:i], s.order[i+1:]...)
				break
			}
		}
	}
	tails := 0
	contended := false
	for oldKey, ref := range s.entries {
		if ref.chatID == chatID && ref.account == account {
			tails = ref.tails + 1
			delete(s.entries, oldKey) // 只保留该会话的最新键
		} else if oldKey == key {
			// 同一历史哈希被另一会话(不同 chatID/账号)注册:
			// 会话身份翻转——先到会话的续接会串线到本会话。
			// 标记 contended,命中一律回退全量(单向安全)。
			contended = true
			delete(s.entries, oldKey)
		}
	}
	s.entries[key] = &sessionRef{account: account, chatID: chatID, msgsLen: msgsLen, tails: tails, contended: contended}
	s.order = append(s.order, key)
}

// drop 清除条目（续接回合失败后，上游会话状态不可信，回退全量）。
func (s *sessionIndex) drop(key string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.entries, key)
}

// tailAnchorIndex 计算续接增量起点：工具循环的 tail 通常为
// [assistant(tool_calls), tool 结果…]，其中不含 user，模型会以为
// 「没有用户指令」而反问（实测连续三轮）。起点回退到最近的 user 消息，
// 指令随增量保留；多数轮次仅多带一条 user，成本可忽略。
func tailAnchorIndex(msgs []openAIMessage, k int) int {
	if k > len(msgs) {
		k = len(msgs)
	}
	for i := k; i < len(msgs); i++ {
		if msgs[i].Role == "user" {
			return i
		}
	}
	// tail 内无 user：回退到 k 之前最近的 user（含匹配历史里的指令）
	for i := k - 1; i >= 0; i-- {
		if msgs[i].Role == "user" {
			return i
		}
	}
	return k
}
