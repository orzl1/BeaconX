package policy

import "testing"

// TestActionValid 覆盖三种合法裁决与若干非法取值。
func TestActionValid(t *testing.T) {
	for _, a := range []Action{Allow, Ask, Deny} {
		if !a.Valid() {
			t.Errorf("裁决 %q 应合法", string(a))
		}
	}
	invalid := []Action{"", "Allow", "ALLOW", "allow ", "block", "approved"}
	for _, a := range invalid {
		if a.Valid() {
			t.Errorf("裁决 %q 应非法", string(a))
		}
	}
}

// TestActionString 校验中文名称与未知取值的原文回退。
func TestActionString(t *testing.T) {
	want := map[Action]string{Allow: "允许", Ask: "询问", Deny: "拒绝"}
	for action, name := range want {
		if got := action.String(); got != name {
			t.Errorf("裁决 %q 的中文名 = %q，期望 %q", string(action), got, name)
		}
	}
	for _, unknown := range []Action{"", "block"} {
		if got := unknown.String(); got != string(unknown) {
			t.Errorf("未知裁决 %q 应回退为原值，实际 %q", string(unknown), got)
		}
	}
}

// TestRiskValid 覆盖三种合法风险等级与若干非法取值。
func TestRiskValid(t *testing.T) {
	for _, r := range []Risk{RiskLow, RiskMedium, RiskHigh} {
		if !r.Valid() {
			t.Errorf("风险等级 %q 应合法", string(r))
		}
	}
	invalid := []Risk{"", "Low", "LOW", "critical", "info", "high "}
	for _, r := range invalid {
		if r.Valid() {
			t.Errorf("风险等级 %q 应非法", string(r))
		}
	}
}

// TestRiskString 校验中文名称与未知取值的原文回退。
func TestRiskString(t *testing.T) {
	want := map[Risk]string{RiskLow: "低", RiskMedium: "中", RiskHigh: "高"}
	for risk, name := range want {
		if got := risk.String(); got != name {
			t.Errorf("风险等级 %q 的中文名 = %q，期望 %q", string(risk), got, name)
		}
	}
	for _, unknown := range []Risk{"", "critical"} {
		if got := unknown.String(); got != string(unknown) {
			t.Errorf("未知风险等级 %q 应回退为原值，实际 %q", string(unknown), got)
		}
	}
}
