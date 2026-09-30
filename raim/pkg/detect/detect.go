// Package detect 实现单历元 RAIM 的故障检测（FDE）与单星排除：
//
//   - 加权残差平方和 SSE=Σ(r_i/σ_i)² 在 H0 下服从 χ²(n-4)；
//   - 阈值 T 由虚警概率 Pfa 确定：P(χ²(n-4)>T)=Pfa；
//   - 少于 5 星只有定位、无冗余，完好性不可用；
//   - 检出后逐星剔一重解重检，通过检验且“只有剔这颗能解释故障”才排除成功；
//   - 少于 6 星无法排除。
package detect

import (
	"raim/pkg/chisq"
	"raim/pkg/lsq"
)

// Mode 是单历元检测状态。
type Mode string

const (
	ModeOK          Mode = "ok"          // 定位+检验通过
	ModeUnavailable Mode = "unavailable" // <5 星：只定位，完好性不可用
	ModeDetected    Mode = "detected"    // 检出但无法排除（<6 星或多星可疑）
	ModeExcluded    Mode = "excluded"    // 检出并成功排除单星
)

// SatResult 是参与解算的单星残差信息。
type SatResult struct {
	ID     int     `json:"id"`
	Resid  float64 `json:"resid"`  // 米
	Sigma  float64 `json:"sigma"`  // 米
	StdRes float64 `json:"stdres"` // 标准化残差 r/σ
}

// Trial 是一次剔星试解的结果。
type Trial struct {
	ExcludedID int     `json:"excluded_id"`
	SSE        float64 `json:"sse"`
	Passes     bool    `json:"passes"`
	Used       int     `json:"used"`
}

// Assessment 是一个历元的单历元检测结论。
type Assessment struct {
	Mode Mode `json:"mode"`

	Used         int         `json:"used"`          // 实际参与解算的星数
	DOF          int         `json:"dof"`           // 全解自由度 n-4（<5 星时为 0 或负，视为无冗余）
	SolDOF       int         `json:"sol_dof"`       // 报告位置解的自由度（排除成功后为 n-1-4）
	Threshold    float64     `json:"threshold"`     // 本历元全解阈值（Pfa 与 dof 决定）
	SolThreshold float64     `json:"sol_threshold"` // 报告解对应门限（排除成功后按 n-5）
	SSE          float64     `json:"sse"`           // 检验统计量
	SatResults   []SatResult `json:"sat_results"`

	// 排除信息
	ExcludedID int     `json:"excluded_id,omitempty"`
	Trials     []Trial `json:"trials,omitempty"`
	Reason     string  `json:"reason,omitempty"` // detected/unavailable 的原因

	// 解（ModeOK/Unavailable 为全解；Excluded 为剔星后的干净解；Detected 为全解）
	Sol *lsq.Solution `json:"-"`
}

// Options 为检测参数。
type Options struct {
	Pfa float64 // 虚警概率
}

// Assess 对一个已通过字段校验的历元做定位、检测、（必要时）排除。
func Assess(ep *lsq.Epoch, opt Options) (*Assessment, error) {
	sol, err := lsq.Solve(ep)
	if err != nil {
		return nil, err
	}
	n := len(ep.Sats)
	a := &Assessment{Used: n, Sol: sol}
	for i, s := range ep.Sats {
		a.SatResults = append(a.SatResults, SatResult{
			ID: s.ID, Resid: sol.Resid[i], Sigma: s.Sigma,
			StdRes: sol.Resid[i] / s.Sigma,
		})
	}
	sse, dof := lsq.WeightedSSE(sol.Resid, sol.Sigma)
	a.SSE, a.DOF, a.SolDOF = sse, dof, dof

	if n < 5 {
		a.Mode = ModeUnavailable
		a.SolDOF = 0
		a.SolThreshold = 0
		a.Reason = "少于 5 颗星，无冗余观测量，只能定位、完好性不可用"
		a.Threshold = 0
		return a, nil
	}
	a.Threshold = chisq.Threshold(dof, opt.Pfa)
	a.SolThreshold = a.Threshold
	if sse <= a.Threshold {
		a.Mode = ModeOK
		return a, nil
	}

	// 检出故障：少于 6 星无法排除（剔后只剩 4 颗，无冗余再检）
	if n < 6 {
		a.Mode = ModeDetected
		a.Reason = "检出故障但少于 6 颗星，无法排除"
		return a, nil
	}

	// 逐星剔一重解重检
	var passing []Trial
	for idx, s := range ep.Sats {
		sub := removeSat(ep, idx)
		subSol, err := lsq.Solve(sub)
		if err != nil {
			// 剔后几何奇异：该试解视为不能解释故障
			a.Trials = append(a.Trials, Trial{ExcludedID: s.ID, Used: n - 1, Passes: false})
			continue
		}
		subSSE, subDOF := lsq.WeightedSSE(subSol.Resid, subSol.Sigma)
		thr := chisq.Threshold(subDOF, opt.Pfa)
		ok := subSSE <= thr
		tr := Trial{ExcludedID: s.ID, SSE: subSSE, Passes: ok, Used: n - 1}
		a.Trials = append(a.Trials, tr)
		if ok {
			passing = append(passing, tr)
			// 记录第一个通过的干净解及其自由度；唯一性通过后再用
			if len(passing) == 1 {
				a.Sol = subSol
				a.SolDOF = subDOF
				a.SolThreshold = thr
			}
		}
	}
	switch len(passing) {
	case 1:
		a.Mode = ModeExcluded
		a.ExcludedID = passing[0].ExcludedID
	default:
		a.Mode = ModeDetected
		if len(passing) == 0 {
			a.Reason = "检出故障但没有任何单星剔法能通过检验（可能多星故障）"
		} else {
			a.Reason = "多颗星剔后都能通过检验，无法唯一识别故障星，按检测失败处理"
		}
		// 多解不唯一时不能使用剔星解，恢复为全解
		sol2, _ := lsq.Solve(ep)
		a.Sol = sol2
		a.SolDOF = dof
		a.SolThreshold = a.Threshold
	}
	return a, nil
}

func removeSat(ep *lsq.Epoch, idx int) *lsq.Epoch {
	sats := make([]lsq.Satellite, 0, len(ep.Sats)-1)
	sats = append(sats, ep.Sats[:idx]...)
	sats = append(sats, ep.Sats[idx+1:]...)
	return &lsq.Epoch{Approx: ep.Approx, Sats: sats}
}
