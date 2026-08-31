// 腾讯计费面测试（SPEC §24.2 落地）：每日签到幂等语义、积分余额聚合规则。
package upstream

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"omnigate2api/internal/auth"
)

func billingAuth() *auth.Auth {
	return &auth.Auth{UserID: "u9", CloudDragonTok: "tok", EnterpriseID: "e9", Domain: "www.codebuddy.cn"}
}

func TestTencentDailyCheckinIdempotent(t *testing.T) {
	var gotTenant, gotAuthz string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthz = r.Header.Get("Authorization")
		gotTenant = r.Header.Get("X-Tenant-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":10001,"msg":"今天已签到"}`))
	}))
	defer srv.Close()
	t.Setenv("OMNIGATE_BILLING_BASE", srv.URL)
	c := NewTencent(5 * time.Second)
	if err := c.DailyCheckin(billingAuth()); err != nil {
		t.Fatalf("already-checked-in must be idempotent success: %v", err)
	}
	if gotAuthz != "Bearer tok" || gotTenant != "e9" {
		t.Fatalf("headers authz=%q tenant=%q", gotAuthz, gotTenant)
	}
}

// 真实形态（8f 实测）：已签到 = HTTP 400 + code 10001 → 幂等成功。
func TestTencentDailyCheckin400Idempotent(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":10001,"msg":"今天已签到，请明天再来"}`))
	}))
	defer srv.Close()
	t.Setenv("OMNIGATE_BILLING_BASE", srv.URL)
	c := NewTencent(5 * time.Second)
	if err := c.DailyCheckin(billingAuth()); err != nil {
		t.Fatalf("400+10001 must be idempotent success: %v", err)
	}
}

func TestTencentDailyCheckinBusinessError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":20001,"msg":"login expired"}`))
	}))
	defer srv.Close()
	t.Setenv("OMNIGATE_BILLING_BASE", srv.URL)
	c := NewTencent(5 * time.Second)
	if err := c.DailyCheckin(billingAuth()); err == nil || !strings.Contains(err.Error(), "login expired") {
		t.Fatalf("business error must surface: %v", err)
	}
}

func TestTencentUserResourceAggregate(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Response":{"Data":{"Accounts":[
		  {"CapacityRemain":100,"CapacityUsed":50,"CycleCapacitySize":0,"CycleCapacityRemain":0,"CycleCapacityUsed":0},
		  {"CapacityRemain":999,"CapacityUsed":0,"CycleCapacitySize":200,"CycleCapacityRemain":150,"CycleCapacityUsed":30},
		  {"CapacityRemain":5,"CapacityUsed":5,"CycleCapacitySize":0,"CycleCapacityRemain":30,"CycleCapacityUsed":0}
		]}}}`))
	}))
	defer srv.Close()
	t.Setenv("OMNIGATE_BILLING_BASE", srv.URL)
	c := NewTencent(5 * time.Second)
	remain, err := c.UserResource(billingAuth())
	if err != nil {
		t.Fatal(err)
	}
	// 规则：CycleCapacitySize>0 → CycleRemain(150)；CycleRemain>0 → Remain(30)；否则 CapacityRemain(100)
	if remain != 280 {
		t.Fatalf("aggregate rule: got %d want 280", remain)
	}
}

func TestTencentUserResourceClampZero(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"Response":{"Data":{"Accounts":[
		  {"CapacityRemain":-10,"CycleCapacitySize":0,"CycleCapacityRemain":0,"CycleCapacityUsed":0}
		]}}}`))
	}))
	defer srv.Close()
	t.Setenv("OMNIGATE_BILLING_BASE", srv.URL)
	c := NewTencent(5 * time.Second)
	remain, err := c.UserResource(billingAuth())
	if err != nil {
		t.Fatal(err)
	}
	if remain != 0 {
		t.Fatalf("negative must clamp to 0: %d", remain)
	}
}
