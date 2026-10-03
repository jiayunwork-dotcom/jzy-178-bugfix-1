// Package statem 实现跨历元的两类持续性状态：
//
//  1. 故障星隔离/恢复计数（卫星级）：
//     被排除的星进入隔离；此后须连续 MinEpochs 个历元通过恢复检验
//     （在不含本星、也不含其他隔离星的干净基线上的留一预测检验，
//     且加回后整体检验通过）才具备放回资格。星不可见或任一历元不通过，
//     计数清零重来；别的星被新排除不清零本星计数。多颗同历元攒满时，
//     由上层 BeginEpoch/Qualifying/Release/ResetStreak 按“逐个放回、
//     放回后整体复核”编排，本包不做 GNSS 计算。
//
//  2. 告警状态（系统级）：
//     - snapshot：一个历元超限即告警，下一个历元正常即撤警；
//     - persistence：连续 ConfirmEpochs 超限才告警，连续 ClearEpochs 正常才撤警；
//     - combined：普通超限走窗口，恶劣超限（达 GrossFactor 倍）立即告警，
//     撤警仍走窗口（保证恢复确认）。
//
// 状态机本身不做任何 GNSS 计算，输入是上层算好的每历元事件，便于单测与复用。
package statem

import (
	"sort"

	"raim/pkg/profile"
)

// IsolationEntry 记录一颗隔离星的恢复计数。
type IsolationEntry struct {
	ID            int `json:"id"`
	NormalStreak  int `json:"normal_streak"`   // 连续正常（且加回通过）历元数
	SinceEpochSeq int `json:"since_epoch_seq"` // 从第几个历元开始隔离
}

// AlertMachine 是告警持续状态机。
type AlertMachine struct {
	Active     bool `json:"active"`
	BadStreak  int  `json:"bad_streak"`
	GoodStreak int  `json:"good_streak"`
	// 本告警片段内是否已经拉起过告警（用于误警片段记账）
}

// State 是会话内跨历元的全部持久状态。
type State struct {
	Isolated map[int]*IsolationEntry `json:"isolated"`
	Alert    AlertMachine            `json:"alert"`
}

// NewState 创建空状态。
func NewState() State {
	return State{Isolated: map[int]*IsolationEntry{}}
}

// SatRecovery 是上层对某颗隔离星在本历元的恢复评估结果。
type SatRecovery struct {
	ID     int
	Normal bool // 本星预测检验正常 且 加回后整体检验通过
}

// SatEval 是上层对某颗隔离星在本历元的完整恢复评估（多星隔离用）。
type SatEval struct {
	ID     int
	Normal bool    // 本星预测检验与加回后整体检验是否都通过
	Z      float64 // 本星标准化预测检验量（留一解上的预测残差/预测误差标准差）
}

// BeginEpoch 推进一个历元的隔离状态：
//
//	excludedNow: 本历元新被唯一排除的星（0 表示无）——进入隔离，计数清零；
//	evals:       对“此前已隔离且本历元可见”的星的逐星恢复评估；
//	visible:     本历元可见星集合（隔离星不可见则计数清零）；
//	seq:         历元序号（仅用于记录起始）。
//
// 本方法只推进计数，不解除任何星的隔离——是否放回由上层按
// “同历元逐个放回、放回后整体复核”的策略调用 Release 决定，
// 以保证几颗星同时攒满历时不会未经联合复核一起进解。
//
// 返回本历元仍处于隔离的星编号集合。
func (s *State) BeginEpoch(prof profile.Profile, excludedNow int, evals []SatEval,
	visible map[int]bool, seq int) map[int]bool {

	if excludedNow != 0 && s.Isolated[excludedNow] == nil {
		s.Isolated[excludedNow] = &IsolationEntry{ID: excludedNow, SinceEpochSeq: seq}
	} else if excludedNow != 0 {
		// 已在隔离中的星本历元又被唯一排除：计数清零、起始历元更新
		e := s.Isolated[excludedNow]
		e.NormalStreak = 0
		e.SinceEpochSeq = seq
	}
	evalMap := map[int]SatEval{}
	for _, e := range evals {
		evalMap[e.ID] = e
	}
	still := map[int]bool{}
	for id, e := range s.Isolated {
		if id == excludedNow {
			e.NormalStreak = 0
			still[id] = true
			continue
		}
		if !visible[id] {
			e.NormalStreak = 0 // 不可见，连续性中断
			still[id] = true
			continue
		}
		if ev, ok := evalMap[id]; ok && ev.Normal {
			e.NormalStreak++
		} else {
			e.NormalStreak = 0
		}
		// 计数达标不立即放回：由上层按逐个放回策略决定
		still[id] = true
	}
	return still
}

// Qualifying 返回当前已连续正常达 MinEpochs 个历元、候选放回的星编号（升序）。
func (s *State) Qualifying(prof profile.Profile) []int {
	var ids []int
	for id, e := range s.Isolated {
		if e.NormalStreak >= prof.Isolation.MinEpochs {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

// Release 把一颗星从隔离集合移除（本历元放回）。
func (s *State) Release(id int) {
	delete(s.Isolated, id)
}

// ResetStreak 把一颗隔离星的连续正常计数清零（放回后的联合复核）。
func (s *State) ResetStreak(id int) {
	if e, ok := s.Isolated[id]; ok {
		e.NormalStreak = 0
	}
}

// UpdateIsolation 推进卫星隔离状态（兼容单星接口：达标即放回）。
//
//	excludedNow: 本历元新被排除的星（0 表示无）——进入隔离，计数清零。
//	recovery:   对当前已隔离且本历元可见的星的评估。
//	visible:    本历元可见星集合（隔离星不可见则计数清零）。
//	seq:        历元序号（仅用于记录起始）。
//
// 返回本历元仍处于隔离的星编号集合。
func (s *State) UpdateIsolation(prof profile.Profile, excludedNow int, recovery []SatRecovery,
	visible map[int]bool, seq int) map[int]bool {

	evals := make([]SatEval, 0, len(recovery))
	for _, r := range recovery {
		evals = append(evals, SatEval{ID: r.ID, Normal: r.Normal})
	}
	still := s.BeginEpoch(prof, excludedNow, evals, visible, seq)
	for _, id := range s.Qualifying(prof) {
		if id == excludedNow {
			continue
		}
		s.Release(id)
		delete(still, id)
	}
	return still
}

// EpochStatus 是上层给出的本历元完好性状态。
type EpochStatus struct {
	// IntegrityAvailable 单历元 RAIM 是否可用（星数足够且定位成功）。
	IntegrityAvailable bool
	// IntegrityBad 检测失败（检出无法排除）或 HPL>HAL。
	IntegrityBad bool
	// IdentifiedRisk 本历元确实识别出风险（检出故障，含可/不可排除）。
	// 用于把告警片段区分为“真故障告警”与“误警”。
	IdentifiedRisk bool
	// Gross 恶劣超限（统计量≥GrossFactor×阈值 或 HPL≥GrossFactor×HAL）。
	Gross bool
}

// StepAlert 推进告警状态机，返回本历元告警是否激活。
func (s *State) StepAlert(prof profile.Profile, st EpochStatus) bool {
	m := &s.Alert
	switch prof.Alert.Mode {
	case profile.Snapshot:
		m.Active = st.IntegrityBad
		m.BadStreak = 0
		m.GoodStreak = 0
		if m.Active {
			m.BadStreak = 1
		} else {
			m.GoodStreak = 1
		}
	case profile.Persistence:
		m.stepWindow(prof, st, false)
	case profile.Combined:
		m.stepWindow(prof, st, st.Gross)
	}
	return m.Active
}

func (m *AlertMachine) stepWindow(prof profile.Profile, st EpochStatus, immediate bool) {
	if st.IntegrityBad {
		m.BadStreak++
		m.GoodStreak = 0
		if immediate || m.BadStreak >= prof.Alert.ConfirmEpochs {
			m.Active = true
		}
	} else {
		m.GoodStreak++
		m.BadStreak = 0
		if m.Active && m.GoodStreak >= prof.Alert.ClearEpochs {
			m.Active = false
		}
	}
}
