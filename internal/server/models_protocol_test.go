// 标准供应商等价面：模型信息的三种取用方式（SPEC §29.3 协议分流）。
//
// 对标"正常供应商"该有的行为：
//   - OpenAI：GET /v1/models（列表）+ GET /v1/models/{id}（检索单个）；
//   - Anthropic：GET /v1/models 是**另一种信封**（data[].type/display_name/created_at
//   - has_more/first_id/last_id），且鉴权头是 x-api-key 而非 Bearer。
//
// 同一路径按调用方协议分流；缺任何一项都会让对应客户端的模型自动发现直接失败。
package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"omnigate2api/internal/auth"
)

func getJSON(t *testing.T, srv *httptest.Server, path string, headers map[string]string) (map[string]any, int) {
	t.Helper()
	req, _ := http.NewRequest("GET", srv.URL+path, nil)
	req.Header.Set("Authorization", "Bearer test-key")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	return out, resp.StatusCode
}

func newModelInfoServer(t *testing.T) *httptest.Server {
	t.Helper()
	srv, _, _, _ := buildTestServer(t, "http://127.0.0.1:1", []*auth.Auth{fakeAuth("u1", "tok1")})
	return srv
}

// OpenAI 列表：object=list，条目含 id/object/created/owned_by（标准字段齐备）。
func TestModelInfoOpenAIList(t *testing.T) {
	srv := newModelInfoServer(t)
	body, code := getJSON(t, srv, "/v1/models", nil)
	if code != 200 || body["object"] != "list" {
		t.Fatalf("list shape: code=%d object=%v", code, body["object"])
	}
	data, _ := body["data"].([]any)
	if len(data) == 0 {
		t.Fatal("model list must not be empty")
	}
	first, _ := data[0].(map[string]any)
	for _, k := range []string{"id", "object", "created", "owned_by"} {
		if _, ok := first[k]; !ok {
			t.Fatalf("OpenAI model entry missing %q: %v", k, first)
		}
	}
}

// OpenAI 检索单个：命中 200 且 id 一致；未注册 404 + model_not_found（与 §29 C1 一致）。
func TestModelInfoOpenAIRetrieve(t *testing.T) {
	srv := newModelInfoServer(t)
	body, code := getJSON(t, srv, "/v1/models/glm-5.2", nil)
	if code != 200 || body["id"] != "glm-5.2" {
		t.Fatalf("retrieve: code=%d id=%v", code, body["id"])
	}
	body, code = getJSON(t, srv, "/v1/models/no-such-model", nil)
	if code != 404 {
		t.Fatalf("unknown model must 404, got %d", code)
	}
	e, _ := body["error"].(map[string]any)
	if e["code"] != "model_not_found" {
		t.Fatalf("error envelope: %v", body)
	}
}

// Anthropic 列表：按 anthropic-version 分流，给出 Anthropic 信封与标准字段。
func TestModelInfoAnthropicList(t *testing.T) {
	srv := newModelInfoServer(t)
	h := map[string]string{"anthropic-version": "2023-06-01"}
	body, code := getJSON(t, srv, "/v1/models", h)
	if code != 200 {
		t.Fatalf("code=%d", code)
	}
	if _, ok := body["object"]; ok {
		t.Fatalf("Anthropic caller must not get the OpenAI envelope: %v", body)
	}
	for _, k := range []string{"data", "has_more", "first_id", "last_id"} {
		if _, ok := body[k]; !ok {
			t.Fatalf("Anthropic envelope missing %q: %v", k, body)
		}
	}
	data, _ := body["data"].([]any)
	if len(data) == 0 || len(data) > 20 {
		t.Fatalf("default limit must be 20, got %d", len(data))
	}
	first, _ := data[0].(map[string]any)
	if first["type"] != "model" {
		t.Fatalf("entry type must be model: %v", first)
	}
	if first["display_name"] == "" || first["created_at"] == "" {
		t.Fatalf("display_name/created_at required: %v", first)
	}
	if !strings.Contains(first["created_at"].(string), "T") {
		t.Fatalf("created_at must be RFC3339: %v", first["created_at"])
	}
}

// Anthropic 列表分页：limit 生效且 has_more 正确；游标越界/非法 limit 报 400。
func TestModelInfoAnthropicPagination(t *testing.T) {
	srv := newModelInfoServer(t)
	h := map[string]string{"anthropic-version": "2023-06-01"}

	page1, _ := getJSON(t, srv, "/v1/models?limit=5", h)
	d1, _ := page1["data"].([]any)
	if len(d1) != 5 || page1["has_more"] != true {
		t.Fatalf("limit=5: n=%d has_more=%v", len(d1), page1["has_more"])
	}
	last := page1["last_id"].(string)

	page2, _ := getJSON(t, srv, "/v1/models?limit=5&after_id="+last, h)
	d2, _ := page2["data"].([]any)
	if len(d2) == 0 {
		t.Fatal("after_id must advance the window")
	}
	if d2[0].(map[string]any)["id"] == d1[0].(map[string]any)["id"] {
		t.Fatalf("after_id did not advance: %v", d2[0])
	}
	// before_id 取该 id 之前的条目
	pageB, _ := getJSON(t, srv, "/v1/models?limit=3&before_id="+last, h)
	if db, _ := pageB["data"].([]any); len(db) == 0 {
		t.Fatal("before_id must return earlier items")
	}

	if _, code := getJSON(t, srv, "/v1/models?limit=0", h); code != 400 {
		t.Fatalf("limit=0 must 400, got %d", code)
	}
	if _, code := getJSON(t, srv, "/v1/models?after_id=nope", h); code != 400 {
		t.Fatalf("unknown after_id must 400, got %d", code)
	}
}

// Anthropic 检索单个：Anthropic 形状 + Anthropic 错误信封。
func TestModelInfoAnthropicRetrieve(t *testing.T) {
	srv := newModelInfoServer(t)
	h := map[string]string{"anthropic-version": "2023-06-01"}
	body, code := getJSON(t, srv, "/v1/models/glm-5.2", h)
	if code != 200 || body["type"] != "model" || body["id"] != "glm-5.2" {
		t.Fatalf("retrieve: code=%d body=%v", code, body)
	}
	body, code = getJSON(t, srv, "/v1/models/nope", h)
	if code != 404 {
		t.Fatalf("unknown model must 404, got %d", code)
	}
	if body["type"] != "error" {
		t.Fatalf("Anthropic error envelope required: %v", body)
	}
	e, _ := body["error"].(map[string]any)
	if e["type"] != "not_found_error" {
		t.Fatalf("error type: %v", body)
	}
}

// 鉴权：标准供应商两种头都认（Anthropic 只发 x-api-key；只认 Bearer 会让其全 401）。
func TestAuthAcceptsAnthropicAPIKey(t *testing.T) {
	srv := newModelInfoServer(t) // buildTestServer 设了 APIKey=test-key

	req, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req.Header.Set("x-api-key", "test-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("x-api-key must authenticate, got %d", resp.StatusCode)
	}

	req2, _ := http.NewRequest("GET", srv.URL+"/v1/models", nil)
	req2.Header.Set("x-api-key", "wrong-key")
	resp2, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	resp2.Body.Close()
	if resp2.StatusCode != 401 {
		t.Fatalf("wrong x-api-key must 401, got %d", resp2.StatusCode)
	}
}
