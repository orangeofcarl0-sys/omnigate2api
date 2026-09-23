// cmd/probe 实证辅助：活动域/计费域原样请求（生产路径不经此）。
package upstream

import (
	"net/http"

	"omnigate2api/internal/auth"
)

// DebugGet 活动域 GET 原样返回（cmd/probe 实证用；生产路径不经此）。
func (c *TencentClient) DebugGet(acct *auth.Auth, path string) ([]byte, int, error) {
	return c.petRequest(acct, http.MethodGet, path, nil)
}

// DebugPost 计费域 POST {} 原样返回（cmd/probe 实证用；生产路径不经此）。
func (c *TencentClient) DebugPost(acct *auth.Auth, path string) ([]byte, int, error) {
	return c.billingPost(acct, path, []byte("{}"))
}

// DebugPostBody 活动域 POST 指定 body 原样返回（cmd/probe 实证用）。
func (c *TencentClient) DebugPostBody(acct *auth.Auth, path, body string) ([]byte, int, error) {
	if body == "" {
		body = "{}"
	}
	return c.petRequest(acct, http.MethodPost, path, []byte(body))
}
