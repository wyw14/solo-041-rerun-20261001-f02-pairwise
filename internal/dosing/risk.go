package dosing

import (
	"fmt"
	"math"
)

// RiskLevel 描述一次加药速率变更的风险等级。
type RiskLevel string

const (
	// RiskNormal 普通范围内变更，保持便捷操作，直接下发。
	RiskNormal RiskLevel = "normal"
	// RiskHigh 高风险变更，必须先申请、审核通过后才能进入设备总线。
	RiskHigh RiskLevel = "high"
)

// RiskPolicy 定义高风险剂量变更的判定规则。
//
// 命中以下任一条件即视为高风险：
//   - 新速率达到或超过 RateCeiling（绝对投加上限）；
//   - 存在在运旧值，且相对变化幅度超过 RelativeThreshold；
//   - 该药剂为首次设定（旧值为 0），且新速率超过 AbsoluteStep。
type RiskPolicy struct {
	RelativeThreshold float64 `json:"relative_threshold"`
	AbsoluteStep      float64 `json:"absolute_step"`
	RateCeiling       float64 `json:"rate_ceiling"`
}

// DefaultRiskPolicy 返回厂内默认的高风险判定阈值。
func DefaultRiskPolicy() RiskPolicy {
	return RiskPolicy{RelativeThreshold: 0.25, AbsoluteStep: 20, RateCeiling: 120}
}

// Classify 依据旧值与新值判定风险等级，并返回命中规则的人类可读说明。
func (p RiskPolicy) Classify(oldRate, newRate float64) (RiskLevel, []string) {
	reasons := make([]string, 0, 2)
	if p.RateCeiling > 0 && newRate >= p.RateCeiling {
		reasons = append(reasons, fmt.Sprintf("新速率 %.3f 达到投加上限 %.3f", newRate, p.RateCeiling))
	}
	switch {
	case oldRate > 0:
		delta := math.Abs(newRate-oldRate) / oldRate
		if p.RelativeThreshold > 0 && delta > p.RelativeThreshold {
			reasons = append(reasons, fmt.Sprintf("相对变化 %.0f%% 超过阈值 %.0f%%", delta*100, p.RelativeThreshold*100))
		}
	case p.AbsoluteStep > 0 && newRate > p.AbsoluteStep:
		reasons = append(reasons, fmt.Sprintf("首次投加 %.3f 超过单次限值 %.3f", newRate, p.AbsoluteStep))
	}
	if len(reasons) > 0 {
		return RiskHigh, reasons
	}
	return RiskNormal, nil
}
