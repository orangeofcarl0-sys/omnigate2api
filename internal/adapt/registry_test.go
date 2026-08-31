package adapt

import (
	"os"
	"path/filepath"
	"testing"
)

// SPEC §4.4：内置注册 + 外部 YAML 覆盖同名 + 新增上游 + fail-fast。
func TestRegistryYAML(t *testing.T) {
	dir := t.TempDir()

	// 外部覆盖 codearts 的 trust（由 builtin low → medium）
	override := `id: codearts
session:
  kind: implicit
  trust: medium
limits:
  rate_limit_hints: ["429", "rate limit", "MaaS"]
`
	// 新增第二上游
	echo := `id: echo
message:
  model: roles
session:
  kind: none
limits:
  rate_limit_hints: ["429"]
`
	_ = os.WriteFile(filepath.Join(dir, "a-codearts.yaml"), []byte(override), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "b-echo.yaml"), []byte(echo), 0o644)

	r := NewRegistry(&Codearts)
	if err := r.LoadDir(dir); err != nil {
		t.Fatalf("load: %v", err)
	}
	if got := r.Get("codearts"); got == nil || got.Session.Trust != "medium" {
		t.Fatalf("override failed: %+v", got)
	}
	if got := r.Get("echo"); got == nil || got.Message.Model != "roles" {
		t.Fatalf("new profile not registered: %+v", got)
	}
	if len(r.IDs()) != 2 {
		t.Fatalf("ids: %v", r.IDs())
	}
}

// 非法 YAML / 非法 Profile 必须整体拒绝（fail-fast，不加载部分状态）。
func TestRegistryLoadDirFailFast(t *testing.T) {
	dir := t.TempDir()
	good := `id: good
message:
  model: roles
session:
  kind: none
limits:
  rate_limit_hints: ["429"]
`
	bad := `id: bad` // 校验失败：缺 session/limits
	_ = os.WriteFile(filepath.Join(dir, "good.yaml"), []byte(good), 0o644)
	_ = os.WriteFile(filepath.Join(dir, "bad.yaml"), []byte(bad), 0o644)

	r := NewRegistry(&Codearts)
	if err := r.LoadDir(dir); err == nil {
		t.Fatalf("expected fail-fast for invalid profile")
	}
	if r.Get("good") != nil {
		t.Fatalf("partial state must not be loaded")
	}
}

// 校验失败拒绝注册（Register 幂等安全）。
func TestRegistryRegisterRejectsInvalid(t *testing.T) {
	r := NewRegistry(&Codearts)
	p := Codearts
	p.ID = "dup"
	p.Session.Trust = ""
	if err := r.Register(&p); err == nil {
		t.Fatalf("invalid profile must be rejected")
	}
	if len(r.IDs()) != 1 {
		t.Fatalf("invalid register must not mutate: %v", r.IDs())
	}
}
