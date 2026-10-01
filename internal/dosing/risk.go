package dosing

import "math"

// RiskPolicy 判定加药剂量变更是否属于高风险。
//
// 满足任一条件即为高风险，需要走申请-审核流程：
//   - 相对变化幅度超过 RelativeThreshold（首次投加 oldRate==0 时按绝对增量判定）；
//   - 新设定值超过 AbsoluteLimit（药剂安全上限）。
type RiskPolicy struct {
	RelativeThreshold float64
	AbsoluteLimit     float64
}

// DefaultRiskPolicy 的阈值：相对变化超过 20%，或设定值超过 500 为高风险。
func DefaultRiskPolicy() RiskPolicy {
	return RiskPolicy{RelativeThreshold: 0.20, AbsoluteLimit: 500}
}

func (p RiskPolicy) normalized() RiskPolicy {
	if p.RelativeThreshold <= 0 {
		p.RelativeThreshold = DefaultRiskPolicy().RelativeThreshold
	}
	if p.AbsoluteLimit <= 0 {
		p.AbsoluteLimit = DefaultRiskPolicy().AbsoluteLimit
	}
	return p
}

// IsHighRisk 报告从 oldRate 调整到 newRate 是否需要审批。
func (p RiskPolicy) IsHighRisk(oldRate, newRate float64) bool {
	p = p.normalized()
	if newRate > p.AbsoluteLimit {
		return true
	}
	if oldRate <= 0 {
		// 首次投加：超过安全上限的一半即视为高风险，避免大剂量直接进总线。
		return newRate > p.AbsoluteLimit/2
	}
	deviation := math.Abs(newRate-oldRate) / oldRate
	return deviation > p.RelativeThreshold
}
