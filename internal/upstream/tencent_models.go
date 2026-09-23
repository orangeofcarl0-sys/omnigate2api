// 腾讯模型清单（SPEC §28.4 决策 B）：GET /console/enterprises/personal/models。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"

	"omnigate2api/internal/auth"
)

// ---------------------------------------------------------------------------
// 腾讯模型清单（SPEC §28.4 决策 B：GET /console/enterprises/personal/models）
// ---------------------------------------------------------------------------

// ModelLister 上游模型清单接口（华为/腾讯各自实现；/v1/models 按家族分发）。
type ModelLister interface {
	FetchModels(acct *auth.Auth) ([]ModelInfo, error)
}

// FetchModels 从 /console/enterprises/personal/models 拉取当前账号可用模型：
// envelope {code,data:{models:[...],agents:[...]}}，只暴露 agent 名为 "cli"
// 的模型（对齐 workbuddy2api 实证：CLI 代理绑定清单）。
func (c *TencentClient) FetchModels(acct *auth.Auth) ([]ModelInfo, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for model fetch")
	}
	base, origin := c.resolve(acct.Domain)
	req, err := http.NewRequest(http.MethodGet, base+"/console/enterprises/personal/models", nil)
	if err != nil {
		return nil, err
	}
	tencentCommonHeaders(req, origin)
	if acct.CloudDragonTok != "" {
		req.Header.Set("Authorization", "Bearer "+acct.CloudDragonTok)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, &ApiError{Code: resp.StatusCode, Status: resp.StatusCode,
			Message: truncateStr(string(raw), 200), Path: "/console/enterprises/personal/models"}
	}
	var env struct {
		Code int64 `json:"code"`
		Data struct {
			Models []struct {
				ID               string   `json:"id"`
				Name             string   `json:"name"`
				MaxInputTokens   int64    `json:"maxInputTokens"`
				MaxOutputTokens  int64    `json:"maxOutputTokens"`
				Disabled         bool     `json:"disabled"`
				Vendor           string   `json:"vendor"`
				Tags             []string `json:"tags"`
				SupportsImages   bool     `json:"supportsImages"`
				SupportsToolCall bool     `json:"supportsToolCall"`
				IsDefault        bool     `json:"isDefault"`
				DescriptionZh    string   `json:"descriptionZh"`
			} `json:"models"`
			Agents []struct {
				Name   string   `json:"name"`
				Models []string `json:"models"`
			} `json:"agents"`
		} `json:"data"`
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		return nil, fmt.Errorf("parse models: %w", err)
	}
	if env.Code != 0 {
		return nil, fmt.Errorf("models api business error code=%d", env.Code)
	}
	type modelDetail struct {
		name             string
		maxInputTokens   int64
		maxOutputTokens  int64
		disabled         bool
		vendor           string
		tags             []string
		supportsImages   bool
		supportsToolCall bool
		isDefault        bool
		description      string
	}
	byID := map[string]modelDetail{}
	for _, m := range env.Data.Models {
		byID[m.ID] = modelDetail{m.Name, m.MaxInputTokens, m.MaxOutputTokens, m.Disabled,
			m.Vendor, m.Tags, m.SupportsImages, m.SupportsToolCall, m.IsDefault, m.DescriptionZh}
	}
	out := make([]ModelInfo, 0, 8)
	for _, ag := range env.Data.Agents {
		if ag.Name != "cli" {
			continue
		}
		for _, id := range ag.Models {
			detail, ok := byID[id]
			if !ok || detail.disabled {
				continue
			}
			name := detail.name
			if name == "" {
				name = id
			}
			access, accessLabel, modes := ParseModelTags(detail.tags)
			out = append(out, ModelInfo{
				ID: id, Name: name,
				ContextWindow: detail.maxInputTokens, MaxTokens: detail.maxOutputTokens,
				Vendor: detail.vendor, Modes: modes,
				Access: access, AccessLabel: accessLabel,
				SupportsImages: detail.supportsImages, SupportsTools: detail.supportsToolCall,
				IsDefault: detail.isDefault, Description: detail.description,
			})
		}
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("models api returned empty list for agent cli")
	}
	return out, nil
}
