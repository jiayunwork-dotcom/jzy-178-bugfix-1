package statem_test

import (
	"testing"

	"raim/pkg/profile"
	"raim/pkg/statem"
)

func profWith(mode profile.AlertMode, confirm, clear int, gross float64) profile.Profile {
	p := profile.Profile{
		Name: "t", Pfa: 1e-5, Pmd: 1e-3, HAL: 100,
		Isolation: profile.Isolation{MinEpochs: 3},
		Alert:     profile.Alert{Mode: mode, ConfirmEpochs: confirm, ClearEpochs: clear, GrossFactor: gross},
	}
	return p
}

func TestSnapshotAlertsImmediately(t *testing.T) {
	s := statem.NewState()
	p := profWith(profile.Snapshot, 0, 0, 0)
	if !s.StepAlert(p, statem.EpochStatus{IntegrityAvailable: true, IntegrityBad: true}) {
		t.Fatal("快照模式超限即告警")
	}
	if s.StepAlert(p, statem.EpochStatus{IntegrityAvailable: true, IntegrityBad: false}) {
		t.Fatal("快照模式恢复即撤警")
	}
}

func TestPersistenceWindow(t *testing.T) {
	s := statem.NewState()
	p := profWith(profile.Persistence, 3, 2, 0)
	bad := statem.EpochStatus{IntegrityAvailable: true, IntegrityBad: true}
	good := statem.EpochStatus{IntegrityAvailable: true, IntegrityBad: false}
	// 前两个超限不告警
	if s.StepAlert(p, bad) || s.StepAlert(p, bad) {
		t.Fatal("窗口未满不应告警")
	}
	// 第三个超限拉起
	if !s.StepAlert(p, bad) {
		t.Fatal("连续 3 个应告警")
	}
	// 单个正常不撤
	if !s.StepAlert(p, good) {
		t.Fatal("撤警窗口未满应保持告警")
	}
	// 再一个正常撤警
	if s.StepAlert(p, good) {
		t.Fatal("连续 2 个正常应撤警")
	}
	// 窗口内一次反复应重置计数（防“闪烁”）
	s.StepAlert(p, bad)
	s.StepAlert(p, bad)
	s.StepAlert(p, good) // 打断
	s.StepAlert(p, bad)
	if s.Alert.Active {
		t.Fatal("连续性被正常历元打断，不应告警")
	}
}

func TestCombinedImmediateOnGross(t *testing.T) {
	s := statem.NewState()
	p := profWith(profile.Combined, 3, 2, 2)
	bad := statem.EpochStatus{IntegrityAvailable: true, IntegrityBad: true}
	gross := statem.EpochStatus{IntegrityAvailable: true, IntegrityBad: true, Gross: true}
	if s.StepAlert(p, bad) {
		t.Fatal("普通超限首历元不告警")
	}
	if !s.StepAlert(p, gross) {
		t.Fatal("恶劣超限应立即告警")
	}
}

func TestIsolationRecoveryCount(t *testing.T) {
	s := statem.NewState()
	p := profWith(profile.Persistence, 3, 2, 0)
	vis := map[int]bool{5: true}
	// 第 1 历元排除 5 号星
	still := s.UpdateIsolation(p, 5, nil, vis, 1)
	if !still[5] {
		t.Fatal("刚排除应处于隔离")
	}
	// 连续 2 个历元正常：仍隔离
	for i := 0; i < 2; i++ {
		still = s.UpdateIsolation(p, 0, []statem.SatRecovery{{ID: 5, Normal: true}}, vis, i+2)
	}
	if !still[5] {
		t.Fatal("未满 3 个历元不应恢复")
	}
	// 第 3 个正常历元：恢复
	still = s.UpdateIsolation(p, 0, []statem.SatRecovery{{ID: 5, Normal: true}}, vis, 4)
	if still[5] {
		t.Fatal("连续 3 历元正常应解除隔离")
	}
}

func TestIsolationAbnormalResetsStreak(t *testing.T) {
	s := statem.NewState()
	p := profWith(profile.Persistence, 3, 2, 0)
	vis := map[int]bool{5: true}
	s.UpdateIsolation(p, 5, nil, vis, 1)
	s.UpdateIsolation(p, 0, []statem.SatRecovery{{ID: 5, Normal: true}}, vis, 2)
	// 一个不正常历元：清零
	s.UpdateIsolation(p, 0, []statem.SatRecovery{{ID: 5, Normal: false}}, vis, 3)
	s.UpdateIsolation(p, 0, []statem.SatRecovery{{ID: 5, Normal: true}}, vis, 4)
	still := s.UpdateIsolation(p, 0, []statem.SatRecovery{{ID: 5, Normal: true}}, vis, 5)
	if !still[5] {
		t.Fatal("计数被重置后只连续 2 历元，不应恢复")
	}
}

func TestIsolationInvisibleResetsStreak(t *testing.T) {
	s := statem.NewState()
	p := profWith(profile.Persistence, 3, 2, 0)
	vis := map[int]bool{5: true}
	s.UpdateIsolation(p, 5, nil, vis, 1)
	s.UpdateIsolation(p, 0, []statem.SatRecovery{{ID: 5, Normal: true}}, vis, 2)
	// 该星本历元不可见：计数清零
	s.UpdateIsolation(p, 0, nil, map[int]bool{}, 3)
	s.UpdateIsolation(p, 0, []statem.SatRecovery{{ID: 5, Normal: true}}, vis, 4)
	still := s.UpdateIsolation(p, 0, []statem.SatRecovery{{ID: 5, Normal: true}}, vis, 5)
	if !still[5] {
		t.Fatal("不可见打断连续性，只连续 2 历元不应恢复")
	}
}
