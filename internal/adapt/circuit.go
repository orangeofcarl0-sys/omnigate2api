// CircuitBreaker 守卫熔断器（SPEC §5.3）：
// 增量模式下守卫事件连续触发时，自动降级 native 并告警，冷却后自动复位。
package adapt

import (
	"sync"
	"time"
)

// GuardEvent 守卫事件类型。
type GuardEvent string

const (
	// GuardNoInstruction 模型自述"无上下文/第一条用户消息"类失忆表述。
	GuardNoInstruction GuardEvent = "noinstruction"
	// GuardCross 会话身份翻转导致的续接拒绝（contended/歧义）。
	GuardCross GuardEvent = "cross"
	// GuardMiss incremental 模式下回退 native（重锚定/不健康/歧义等）。
	GuardMiss GuardEvent = "miss"
)

// 缺省参数（SPEC §5.3）：窗口 5 分钟、阈值 3 次、熔断 30 分钟。
const (
	CircuitWindow    = 5 * time.Minute
	CircuitThreshold = 3
	CircuitDuration  = 30 * time.Minute
)

// CircuitBreaker 状态机：窗口内同类事件 ≥ 阈值 → open（强制 native），
// 冷却后自动 close。
type CircuitBreaker struct {
	mu        sync.Mutex
	window    time.Duration
	threshold int
	duration  time.Duration

	events    map[GuardEvent][]int64 // 事件类型 → 发生时刻（窗口滑动）
	openUntil time.Time
	lastEvent GuardEvent
}

// NewCircuitBreaker 构造熔断器。
func NewCircuitBreaker(window time.Duration, threshold int, duration time.Duration) *CircuitBreaker {
	if window <= 0 {
		window = CircuitWindow
	}
	if threshold <= 0 {
		threshold = CircuitThreshold
	}
	if duration <= 0 {
		duration = CircuitDuration
	}
	return &CircuitBreaker{
		window:    window,
		threshold: threshold,
		duration:  duration,
		events:    map[GuardEvent][]int64{},
	}
}

// Record 记录一次守卫事件；返回 true 表示本次记录触发了熔断。
func (c *CircuitBreaker) Record(event GuardEvent, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.prune(now)
	c.events[event] = append(c.events[event], now.UnixNano())
	c.lastEvent = event
	if len(c.events[event]) >= c.threshold {
		c.openUntil = now.Add(c.duration)
		return true
	}
	return false
}

// Open 当前是否处于熔断（强制 native）。
func (c *CircuitBreaker) Open(now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.openUntil.IsZero() && now.After(c.openUntil) {
		c.openUntil = time.Time{}
		c.events = map[GuardEvent][]int64{}
	}
	return now.Before(c.openUntil)
}

// LastEvent 最近事件（诊断用）。
func (c *CircuitBreaker) LastEvent() GuardEvent {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastEvent
}

func (c *CircuitBreaker) prune(now time.Time) {
	cutoff := now.Add(-c.window).UnixNano()
	for ev, ts := range c.events {
		j := 0
		for _, t := range ts {
			if t > cutoff {
				c.events[ev][j] = t
				j++
			}
		}
		c.events[ev] = c.events[ev][:j]
	}
}
