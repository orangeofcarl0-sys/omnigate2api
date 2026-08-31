// 裸模型名路由表测试（SPEC §29）：撞名 fail-fast / 归一化 / Resolve 优先级 /
// 持久化往返。
package adapt

import (
	"os"
	"path/filepath"
	"testing"
)

func TestRouteTableNormalizeAndResolve(t *testing.T) {
	rt, err := NewRouteTable([]ModelRoute{
		{Model: "GLM-5.2", Family: "codearts"},
		{Model: "kimi-k2.7", Family: "workbuddy"},
	})
	if err != nil {
		t.Fatal(err)
	}
	// 小写归一
	if f, ok := rt.FamilyOf("glm-5.2"); !ok || f != "codearts" {
		t.Fatalf("normalize: %q %v", f, ok)
	}
	// Resolve：explicit 优先；表命中；未命中 → codearts
	if got, ok := rt.Resolve("glm-5.2", "workbuddy"); !ok || got != "workbuddy" {
		t.Fatalf("explicit override: %s %v", got, ok)
	}
	if got, ok := rt.Resolve("GLM-5.2", ""); !ok || got != "codearts" {
		t.Fatalf("table hit: %s %v", got, ok)
	}
	if got, ok := rt.Resolve("unknown-model", ""); !ok || got != "codearts" {
		t.Fatalf("fallback: %s %v", got, ok)
	}
}

func TestRouteTableDuplicateFailFast(t *testing.T) {
	_, err := NewRouteTable([]ModelRoute{
		{Model: "glm-5.2", Family: "codearts"},
		{Model: "glm-5.2", Family: "workbuddy"},
	})
	if err == nil {
		t.Fatal("duplicate must fail fast")
	}
}

func TestRouteTableReplaceKeepsOldOnError(t *testing.T) {
	rt, _ := NewRouteTable([]ModelRoute{{Model: "glm-5.2", Family: "codearts"}})
	if err := rt.Replace([]ModelRoute{{Model: "x", Family: "a"}, {Model: "x", Family: "b"}}); err == nil {
		t.Fatal("duplicate replace must fail")
	}
	if got := rt.Routes(); len(got) != 1 || got[0].Model != "glm-5.2" {
		t.Fatalf("table must remain unchanged: %+v", got)
	}
}

func TestRouteTableSaveLoad(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "routes.json")
	rt, _ := NewRouteTable([]ModelRoute{{Model: "hy3", Family: "workbuddy"}})
	if err := SaveRouteTableFile(file, rt); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRouteTableFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if f, ok := got.FamilyOf("hy3"); !ok || f != "workbuddy" {
		t.Fatalf("roundtrip: %q %v", f, ok)
	}
	// 缺文件 → nil 无错
	missing, err := LoadRouteTableFile(filepath.Join(dir, "none.json"))
	if err != nil || missing != nil {
		t.Fatalf("missing must be nil,nil: %v %v", missing, err)
	}
	// 非法文件 → 错误（fail-fast）
	if err := os.WriteFile(file, []byte(`[{"model":`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRouteTableFile(file); err == nil {
		t.Fatal("corrupt file must fail")
	}
}

// C1：禁用集——blocked 模型全局拒绝（显式渠道也不例外）。
func TestRouteTableBlocked(t *testing.T) {
	rt, _ := NewRouteTable([]ModelRoute{{Model: "glm-5.2", Family: "codearts"}, {Model: "kimi-k2.7", Family: "workbuddy"}})
	rt.Block("glm-5.2")
	if f, ok := rt.Resolve("glm-5.2", ""); ok || f != "" {
		t.Fatalf("blocked must deny: %q %v", f, ok)
	}
	if f, ok := rt.Resolve("glm-5.2", "workbuddy"); ok {
		t.Fatalf("blocked must deny even with explicit: %q %v", f, ok)
	}
	if f, ok := rt.Resolve("kimi-k2.7", ""); !ok || f != "workbuddy" {
		t.Fatalf("unblocked must work: %q %v", f, ok)
	}
	rt.Unblock("glm-5.2")
	if f, ok := rt.Resolve("glm-5.2", ""); !ok || f != "codearts" {
		t.Fatalf("unblock must restore: %q %v", f, ok)
	}
	bl := rt.Blocked()
	if len(bl) != 0 {
		t.Fatalf("blocked list must be empty after unblock: %v", bl)
	}
}

// C1：持久化 blocked 往返 + 旧格式（顶层数组）兼容。
func TestRouteTableBlockedPersist(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "routes.json")
	rt, _ := NewRouteTable([]ModelRoute{{Model: "hy3", Family: "workbuddy"}})
	rt.Block("hy3")
	if err := SaveRouteTableFile(file, rt); err != nil {
		t.Fatal(err)
	}
	got, err := LoadRouteTableFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := got.Resolve("hy3", ""); ok {
		t.Fatal("blocked must survive persist")
	}
	if f, _ := got.Resolve("hy3", "workbuddy"); f == "workbuddy" {
		t.Fatal("explicit must not bypass blocked after load")
	}
	// 旧格式：顶层直接数组
	if err := os.WriteFile(file, []byte(`[{"model":"glm-5.2","family":"codearts"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	legacy, err := LoadRouteTableFile(file)
	if err != nil || legacy == nil {
		t.Fatalf("legacy array format must load: %v", err)
	}
}
