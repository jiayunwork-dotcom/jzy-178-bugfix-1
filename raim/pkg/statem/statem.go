// Package statem 实现跨历元的两类持续性状态：
//
//  1. 故障星隔离/恢复计数（卫星级）：
//     被排除的星进入隔离；此后须连续 MinEpochs 个历元“本星残差正常且
//     加回后整体检验通过”才解除。星不可见或任一条件不满足，计数清零重来。
//
//  2. 告警状态（系统级）：
//     - snapshot：一个历元超限即告警，下一个历元正常即撤警；
//     - persistence：连续 ConfirmEpochs 超限才告警，连续 ClearEpochs 正常才撤警；
//     - combined：普通超限走窗口，恶劣超限（达 GrossFactor 倍）立即告警，
//     撤警仍走窗口（保证恢复确认）。
//
// 状态机本身不做任何 GNSS 计算，输入是上层算好的每历元事件，便于单测与复用。
package statem

import "raim/pkg/profile"

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
	Normal bool // 本星残差正常 且 加回后整体检验通过
}

// UpdateIsolation 推进卫星隔离状态。
//
//	excludedNow: 本历元新被排除的星（0 表示无）——进入隔离，计数清零。
//	recovery:   对当前已隔离且本历元可见的星的评估。
//	visible:    本历元可见星集合（隔离星不可见则计数清零）。
//	seq:        历元序号（仅用于记录起始）。
//
// 返回本历元仍处于隔离的星编号集合。
func (s *State) UpdateIsolation(prof profile.Profile, excludedNow int, recovery []SatRecovery,
	visible map[int]bool, seq int) map[int]bool {

	if excludedNow != 0 {
		s.Isolated[excludedNow] = &IsolationEntry{ID: excludedNow, SinceEpochSeq: seq}
	}
	recMap := map[int]bool{}
	for _, r := range recovery {
		recMap[r.ID] = r.Normal
	}
	still := map[int]bool{}
	for id, e := range s.Isolated {
		if id == excludedNow {
			still[id] = true
			continue
		}
		if !visible[id] {
			e.NormalStreak = 0 // 不可见，连续性中断
			still[id] = true
			continue
		}
		if recMap[id] {
			e.NormalStreak++
		} else {
			e.NormalStreak = 0
		}
		if e.NormalStreak >= prof.Isolation.MinEpochs {
			delete(s.Isolated, id) // 满足恢复条件，解除隔离
		} else {
			still[id] = true
		}
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
