package dosing

import "testing"

func TestRiskPolicyClassify(t *testing.T) {
	policy := DefaultRiskPolicy()

	cases := []struct {
		name        string
		oldRate     float64
		newRate     float64
		wantRisk    RiskLevel
		wantReasons int
	}{
		{"首次小幅投加", 0, 10, RiskNormal, 0},
		{"首次大幅投加超限", 0, 25, RiskHigh, 1},
		{"小幅度调整", 100, 110, RiskNormal, 0},
		{"下调保持普通", 100, 80, RiskNormal, 0},
		{"大幅上调", 100, 200, RiskHigh, 1},
		{"达到投加上限", 50, 120, RiskHigh, 1},
		{"超上限且相对变化大", 100, 200, RiskHigh, 1}, // 上限与相对变化同时命中
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			risk, reasons := policy.Classify(tc.oldRate, tc.newRate)
			if risk != tc.wantRisk {
				t.Fatalf("risk = %s, want %s (reasons: %v)", risk, tc.wantRisk, reasons)
			}
			if tc.wantRisk == RiskHigh && len(reasons) == 0 {
				t.Fatal("high risk change must explain the trigger")
			}
			if risk == RiskNormal && len(reasons) != 0 {
				t.Fatalf("normal change must not carry risk reasons: %v", reasons)
			}
		})
	}
}

func TestRiskPolicyCeilingAndRelativeBothTrigger(t *testing.T) {
	policy := DefaultRiskPolicy()
	risk, reasons := policy.Classify(100, 200)
	if risk != RiskHigh {
		t.Fatalf("risk = %s, want high", risk)
	}
	if len(reasons) < 2 {
		t.Fatalf("expected ceiling and relative reasons, got %v", reasons)
	}
}
