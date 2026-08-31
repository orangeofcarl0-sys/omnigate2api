// auth 命名空间测试（SPEC §24.1）：前缀解析 / FileName 带 Profile 前缀。
package auth

import (
	"os"
	"path/filepath"
	"testing"
)

func TestFileNameByProfile(t *testing.T) {
	h := New("u1", "n1", "d1", "tok", "ak", "sk", "2099-01-01T00:00:00Z", "rt", "v")
	if h.FileName() != "codearts-u1.json" {
		t.Fatalf("default prefix: %s", h.FileName())
	}
	h.Profile = "workbuddy"
	if h.FileName() != "workbuddy-u1.json" {
		t.Fatalf("workbuddy prefix: %s", h.FileName())
	}
}

func TestLoadDirNamespaces(t *testing.T) {
	dir := t.TempDir()
	huawei := New("u1", "n1", "d1", "tok1", "ak", "sk", "2099-01-01T00:00:00Z", "rt", "v")
	if err := SaveNew(dir, huawei); err != nil {
		t.Fatal(err)
	}
	tencent := New("u2", "n2", "d2", "tok2", "", "", "2099-01-01T00:00:00Z", "rt2", "")
	tencent.Profile = "workbuddy"
	if err := SaveNew(dir, tencent); err != nil {
		t.Fatal(err)
	}
	// 无关文件应被忽略
	_ = os.WriteFile(filepath.Join(dir, "other.json"), []byte(`{"x":1}`), 0o600)

	auths, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 2 {
		t.Fatalf("loaded=%d want 2", len(auths))
	}
	byProfile := map[string]string{}
	for _, a := range auths {
		byProfile[a.Profile] = a.UserID
	}
	if byProfile["codearts"] != "u1" || byProfile["workbuddy"] != "u2" {
		t.Fatalf("profile mapping: %v", byProfile)
	}
}

func TestSaveNewRoundTripTencent(t *testing.T) {
	dir := t.TempDir()
	a := New("u9", "n9", "d9", "tok9", "", "", "2099-01-01T00:00:00Z", "rt9", "")
	a.Profile = "workbuddy"
	a.EnterpriseID = "e9"
	a.Domain = "www.codebuddy.cn"
	if err := SaveNew(dir, a); err != nil {
		t.Fatal(err)
	}
	auths, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(auths) != 1 {
		t.Fatalf("loaded=%d", len(auths))
	}
	got := auths[0]
	if got.EnterpriseID != "e9" || got.Domain != "www.codebuddy.cn" || got.RefreshToken != "rt9" {
		t.Fatalf("tencent fields lost: %+v", got)
	}
}
