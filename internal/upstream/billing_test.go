// 腾讯计费面测试（SPEC §24.2 落地）：每日签到幂等语义、积分余额聚合规则。
package upstream

import (
	"errors"
	"io"
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
	res, err := c.DailyCheckin(billingAuth())
	if err != nil || res == nil || !res.Already {
		t.Fatalf("already-checked-in must be idempotent success: res=%+v err=%v", res, err)
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
	res, err := c.DailyCheckin(billingAuth())
	if err != nil || res == nil || !res.Already {
		t.Fatalf("400+10001 must be idempotent success: res=%+v err=%v", res, err)
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
	if _, err := c.DailyCheckin(billingAuth()); err == nil || !strings.Contains(err.Error(), "login expired") {
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

// SPEC §32 活动面：签到状态解析（checkin-activity-status）。
func TestTencentCheckinStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasSuffix(r.URL.Path, "/v2/billing/meter/checkin-activity-status") {
			t.Errorf("path=%s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":0,"data":{"theme_name":"加油站","today_checked_in":true,"streak_days":7,"today_credit":100,"total_credits":700}}`))
	}))
	defer srv.Close()
	t.Setenv("OMNIGATE_BILLING_BASE", srv.URL)
	c := NewTencent(5 * time.Second)
	st, err := c.CheckinStatus(billingAuth())
	if err != nil {
		t.Fatal(err)
	}
	if st.ThemeName != "加油站" || !st.TodayCheckedIn || st.StreakDays != 7 || st.TotalCredits != 700 {
		t.Fatalf("status=%+v", st)
	}
}

// SPEC §32 活动面：宠物四端点（status/config/depart/claim）+ no unclaimed 幂等。
func TestTencentPetFlow(t *testing.T) {
	var departBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/travel/status"):
			if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("X-User-Id") != "u9" {
				t.Errorf("pet headers authz=%q uid=%q", r.Header.Get("Authorization"), r.Header.Get("X-User-Id"))
			}
			_, _ = w.Write([]byte(`{"code":0,"data":{"state":"idle","daily_limit_reached":false,"arrive_at":0,"server_now":1788630000}}`))
		case strings.HasSuffix(r.URL.Path, "/travel/config"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"locations":[{"id":1,"name":"森林","duration_hours_min":2,"duration_hours_max":4}]}}`))
		case strings.HasSuffix(r.URL.Path, "/travel/depart"):
			b, _ := io.ReadAll(r.Body)
			departBody = string(b)
			_, _ = w.Write([]byte(`{"code":0,"msg":"ok"}`))
		case strings.HasSuffix(r.URL.Path, "/travel/claim"):
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"code":400,"msg":"no unclaimed reward"}`))
		}
	}))
	defer srv.Close()
	t.Setenv("OMNIGATE_ACTIVITY_BASE", srv.URL)
	c := NewTencent(5 * time.Second)
	a := billingAuth()

	st, err := c.PetTravelStatus(a)
	if err != nil || st.State != "idle" || st.DailyLimitReached {
		t.Fatalf("status=%+v err=%v", st, err)
	}
	locs, err := c.PetTravelConfig(a)
	if err != nil || len(locs) != 1 || locs[0].ID.String() != "1" || locs[0].DurationHoursMax != 4 {
		t.Fatalf("locs=%+v err=%v", locs, err)
	}
	if err := c.PetDepart(a, locs[0].ID); err != nil {
		t.Fatal(err)
	}
	// 数字 id 必须保类型透传（活测实证：真实 API id 为数字）
	if !strings.Contains(departBody, `"location_id":1`) {
		t.Fatalf("depart body=%s", departBody)
	}
	if _, err := c.PetClaim(a, "0"); !errors.Is(err, ErrPetNoUnclaimed) {
		t.Fatalf("no-unclaimed must be idempotent sentinel: %v", err)
	}
}

// SPEC §32.2 补：宠物激活（quota→open）与 claim record_id 契约。
func TestTencentPetActivation(t *testing.T) {
	var claimBody, openBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/buddy/quota"):
			_, _ = w.Write([]byte(`{"code":0,"data":{"affordable":3,"max_open_count":1,"cost_per_open":50}}`))
		case strings.HasSuffix(r.URL.Path, "/buddy/open"):
			b, _ := io.ReadAll(r.Body)
			openBody = string(b)
			_, _ = w.Write([]byte(`{"code":0,"data":{"buddy":{"name":"小星"}}}`))
		case strings.HasSuffix(r.URL.Path, "/travel/claim"):
			b, _ := io.ReadAll(r.Body)
			claimBody = string(b)
			_, _ = w.Write([]byte(`{"code":0,"data":{"credit":88}}`))
		}
	}))
	defer srv.Close()
	t.Setenv("OMNIGATE_ACTIVITY_BASE", srv.URL)
	c := NewTencent(5 * time.Second)
	a := billingAuth()

	q, err := c.PetQuota(a)
	if err != nil || q.Affordable != 3 || q.MaxOpenCount != 1 || q.CostPerOpen != 50 {
		t.Fatalf("quota=%+v err=%v", q, err)
	}
	if err := c.PetOpenBox(a, 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(openBody, `"count":1`) || !strings.Contains(openBody, `"client_token"`) {
		t.Fatalf("open body=%s", openBody)
	}
	if credit, err := c.PetClaim(a, "12345"); err != nil || credit != 88 {
		t.Fatalf("claim credit=%d err=%v", credit, err)
	}
	if !strings.Contains(claimBody, `"record_id":12345`) {
		t.Fatalf("claim body must carry record_id: %s", claimBody)
	}
}

// SPEC §32.6：10001 双语义——全球版「活动未开启」不得当作"已签到成功"。
func TestTencentDailyCheckinInactive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"code":10001,"msg":"签到活动未开启或已过期"}`))
	}))
	defer srv.Close()
	t.Setenv("OMNIGATE_BILLING_BASE", srv.URL)
	c := NewTencent(5 * time.Second)
	res, err := c.DailyCheckin(billingAuth())
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || res.Already || !res.Inactive || res.Reason == "" {
		t.Fatalf("global inactive must be reported as inactive (not already): %+v", res)
	}
}
