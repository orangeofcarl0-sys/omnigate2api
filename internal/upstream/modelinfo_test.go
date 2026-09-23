// 模型目录元数据与访问类别归一（SPEC §29.7）。
package upstream

import "testing"

func TestParseModelTags(t *testing.T) {
	cases := []struct {
		name      string
		tags      []string
		wantKind  string
		wantLabel string
		wantModes []string
	}{
		{"无标签按量计费", nil, AccessPaid, "按量计费", nil},
		{"夜间免费", []string{"craft", "badge:夜间免费:#FF0000"}, AccessNightFree, "夜间免费", []string{"craft"}},
		{"限时免费", []string{"craft", "badge:限时免费:#FF0000"}, AccessLimitedFree, "限时免费", []string{"craft"}},
		{"夜间折扣", []string{"craft", "badge:夜间折扣:#1E90FF"}, AccessDiscount, "夜间折扣", []string{"craft"}},
		{"裸免费标签", []string{"badge:免费:#0F0"}, AccessFree, "免费", nil},
		{"非 badge 标签全部进模式", []string{"craft", "text-to-image"}, AccessPaid, "按量计费", []string{"craft", "text-to-image"}},
		{"未知 badge 文案按量计费但保留原文", []string{"badge:新活动:#123456"}, AccessPaid, "新活动", nil},
		{"空字符串标签忽略", []string{"", "craft"}, AccessPaid, "按量计费", []string{"craft"}},
	}
	for _, c := range cases {
		kind, label, modes := ParseModelTags(c.tags)
		if kind != c.wantKind || label != c.wantLabel {
			t.Fatalf("%s: got (%s,%s) want (%s,%s)", c.name, kind, label, c.wantKind, c.wantLabel)
		}
		if len(modes) != len(c.wantModes) {
			t.Fatalf("%s: modes=%v want %v", c.name, modes, c.wantModes)
		}
		for i := range modes {
			if modes[i] != c.wantModes[i] {
				t.Fatalf("%s: modes=%v want %v", c.name, modes, c.wantModes)
			}
		}
	}
}

func TestAccessForStatic(t *testing.T) {
	if k, l := AccessForStatic("glm-5.3-flash"); k != AccessBenefit || l != "福利额度" {
		t.Fatalf("benefit model: got (%s,%s)", k, l)
	}
	if k, _ := AccessForStatic("glm-5.2"); k != AccessPaid {
		t.Fatalf("classic model must be paid, got %s", k)
	}
}

func TestAccessLabelFallback(t *testing.T) {
	if got := AccessLabel(AccessNightFree); got != "夜间免费" {
		t.Fatalf("label=%s", got)
	}
	if got := AccessLabel("custom-kind"); got != "custom-kind" {
		t.Fatalf("unknown kind should echo back, got %s", got)
	}
}
