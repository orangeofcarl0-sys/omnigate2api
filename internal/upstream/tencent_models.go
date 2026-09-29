// 腾讯模型清单（SPEC §28.4 决策 B）：GET /console/enterprises/personal/models，
// 国际版回退 GET /v3/config。
package upstream

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
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

const tencentConsoleModelsPath = "/console/enterprises/personal/models"

// FetchModels 拉取当前账号可用模型：主路径 `GET /console/enterprises/personal/models`
// （国内控制台），**失败则回退 `GET /v3/config`**。
//
// 为什么要回退（2026-09-29 实测）：**国际版（www.workbuddy.ai）没有那个控制台路径**——
// 直接返回 500 + HTML 错误页，于是面板「模型与路由 → 扫描各账号」对**全部国际账号报错**
// （国内号正常）。而 `/v3/config` 双域都返回同构的 `{models, agents}`，桌面 UA 下还带
// modelPromotions（SPEC §29.7.2）；国际版的 cli agent 清单里就是该账号真正可用的模型。
func (c *TencentClient) FetchModels(acct *auth.Auth) ([]ModelInfo, error) {
	if acct == nil {
		return nil, fmt.Errorf("account required for model fetch")
	}
	list, err := c.fetchModelsConsole(acct)
	if err == nil {
		return list, nil
	}
	realm := "cn"
	if TencentRegion(acct.Domain) {
		realm = "global"
	}
	log.Printf("tencent models: %s failed (%v), falling back to /v3/config realm=%s",
		tencentConsoleModelsPath, err, realm)
	list2, err2 := c.fetchModelsConfig(acct)
	if err2 != nil {
		// 两个端点都失败时把两条错误都带出去（排障一眼看出是哪条路的问题）。
		return nil, fmt.Errorf("%s: %v; /v3/config: %w", tencentConsoleModelsPath, err, err2)
	}
	return list2, nil
}

// fetchModelsConsole 国内控制台清单（原实现）。
func (c *TencentClient) fetchModelsConsole(acct *auth.Auth) ([]ModelInfo, error) {
	base, origin := c.resolve(acct.Domain)
	req, err := http.NewRequest(http.MethodGet, base+tencentConsoleModelsPath, nil)
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
			Message: truncateStr(string(raw), 200), Path: tencentConsoleModelsPath}
	}
	return parseTencentModels(raw)
}

// fetchModelsConfig 回退路径：/v3/config（桌面 UA，与促销解析同一请求形状）。
func (c *TencentClient) fetchModelsConfig(acct *auth.Auth) ([]ModelInfo, error) {
	base, _ := c.resolve(acct.Domain)
	raw, status, err := c.tencentDo(acct, tencentHTTPOpts{
		base: base, method: http.MethodGet, path: "/v3/config", desktopUA: true,
	})
	if err != nil {
		return nil, err
	}
	dumpRawConfig(acct, raw)
	if err := checkBiz("v3-config(models)", status, 0, ""); err != nil {
		return nil, err
	}
	return parseTencentModels(raw)
}

// parseTencentModels 解析 `{code,data:{models:[...],agents:[...]}}`：只暴露 agent 名为
// "cli" 的模型（对齐 workbuddy2api 实证：CLI 代理绑定清单）。两个端点形状同构，共用。
func parseTencentModels(raw []byte) ([]ModelInfo, error) {
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
		return nil, fmt.Errorf("models business error code=%d", env.Code)
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
