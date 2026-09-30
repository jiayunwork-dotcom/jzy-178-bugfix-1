// Package profile 管理 RAIM 完好性运行档（具名参数集）：
// 虚警/漏检概率、水平告警限，以及跨历元的隔离恢复与告警持续性参数。
//
// 内置三档（航路、终端区、非精密进近）的 HAL 取自 ICAO Annex 10 与
// RTCA/WAAS 标准中被广泛引用的数值；持续性窗口参数为本实现给出的工程默认，
// 依据见 docs/design.md。
package profile

import (
	"fmt"
	"strings"

	"raim/pkg/apierr"
)

// AlertMode 告警判定方式。
type AlertMode string

const (
	// Snapshot 逐历元快照判定：任一历元超限立即告警，下一历元恢复立即撤警。
	Snapshot AlertMode = "snapshot"
	// Persistence 持续性窗口：连续 confirm 个历元超限才告警，连续 clear 个才撤警。
	Persistence AlertMode = "persistence"
	// Combined 组合：普通超限走持续性窗口，恶劣超限（达到 grossFactor 倍）立即告警。
	Combined AlertMode = "combined"
)

// Profile 是一个具名运行档。
type Profile struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Pfa         float64   `json:"pfa"` // 虚警概率
	Pmd         float64   `json:"pmd"` // 漏检概率
	HAL         float64   `json:"hal"` // 水平告警限，米
	Isolation   Isolation `json:"isolation"`
	Alert       Alert     `json:"alert"`
	Sources     []string  `json:"sources"` // 参数出处
}

// Isolation 是故障星隔离与恢复参数。
type Isolation struct {
	// MinEpochs 隔离后至少连续多少个历元“本星正常且加回后整体通过”才恢复。
	MinEpochs int `json:"min_epochs"`
}

// Alert 是告警持续性参数。
type Alert struct {
	Mode AlertMode `json:"mode"`
	// ConfirmEpochs 连续多少个历元检测失败/HPL>HAL 才拉起告警。
	ConfirmEpochs int `json:"confirm_epochs"`
	// ClearEpochs 连续多少个历元恢复正常才撤除告警。
	ClearEpochs int `json:"clear_epochs"`
	// GrossFactor 仅 combined：恶劣超限倍数（检测统计量/阈值 或 HPL/HAL），
	// 达到即立即告警，不等持续性窗口。
	GrossFactor float64 `json:"gross_factor,omitempty"`
}

// Validate 校验运行档字段。
func (p *Profile) Validate() error {
	if strings.TrimSpace(p.Name) == "" {
		return apierr.New("name", "运行档名称不能为空")
	}
	if p.Pfa <= 0 || p.Pfa >= 1 {
		return apierr.New("pfa", "虚警概率必须在 (0,1) 内")
	}
	if p.Pmd <= 0 || p.Pmd >= 1 {
		return apierr.New("pmd", "漏检概率必须在 (0,1) 内")
	}
	if p.HAL <= 0 {
		return apierr.New("hal", "水平告警限必须为正（米）")
	}
	if p.Isolation.MinEpochs < 1 {
		return apierr.New("isolation.min_epochs", "隔离恢复至少 1 个历元")
	}
	switch p.Alert.Mode {
	case Snapshot:
		// 窗口参数不使用
	case Persistence:
		if p.Alert.ConfirmEpochs < 1 {
			return apierr.New("alert.confirm_epochs", "持续性确认历元至少为 1")
		}
		if p.Alert.ClearEpochs < 1 {
			return apierr.New("alert.clear_epochs", "撤警确认历元至少为 1")
		}
	case Combined:
		if p.Alert.ConfirmEpochs < 1 || p.Alert.ClearEpochs < 1 {
			return apierr.New("alert", "组合模式仍需确认与撤警历元")
		}
		if p.Alert.GrossFactor <= 1 {
			return apierr.New("alert.gross_factor", "恶劣倍数必须严格大于 1")
		}
	default:
		return apierr.New("alert.mode", fmt.Sprintf("未知告警模式 %q（可选 snapshot/persistence/combined）", p.Alert.Mode))
	}
	return nil
}

// Builtins 返回三档内置运行档。
func Builtins() []Profile {
	return []Profile{
		{
			Name:        "enroute",
			Description: "航路（En-route）：HAL=2.0 nm，持续性窗口 + 恶劣立即告警",
			Pfa:         1e-5,
			Pmd:         1e-3,
			HAL:         3704, // 2.0 海里
			Isolation:   Isolation{MinEpochs: 5},
			Alert:       Alert{Mode: Combined, ConfirmEpochs: 3, ClearEpochs: 3, GrossFactor: 2},
			Sources: []string{
				"ICAO Annex 10 Vol.I (Radio Navigation Aids), GPS RAIM 航路 HAL=2.0 NM",
				"RTCA DO-208 (1991), Minimum Operational Performance Standards for Airborne Supplemental Navigation Equipment Using GPS, §2 RAIM 算法",
				"Groves (2013), Principles of GNSS, Inertial, and Multisensor Integrated Navigation Systems, 2nd ed., Ch.17 (RAIM/FDE)",
			},
		},
		{
			Name:        "terminal",
			Description: "终端区（Terminal）：HAL=1.0 nm",
			Pfa:         1e-5,
			Pmd:         1e-3,
			HAL:         1852, // 1.0 海里
			Isolation:   Isolation{MinEpochs: 5},
			Alert:       Alert{Mode: Combined, ConfirmEpochs: 3, ClearEpochs: 3, GrossFactor: 2},
			Sources: []string{
				"ICAO Annex 10 Vol.I, GPS RAIM 终端区 HAL=1.0 NM",
				"RTCA DO-208 (1991), §2",
				"Kaplan & Hegarty (2017), Understanding GPS/GNSS: Principles and Applications, 3rd ed., Ch.11 (RAIM)",
			},
		},
		{
			Name:        "npa",
			Description: "非精密进近（NPA）：HAL=0.3 nm",
			Pfa:         1e-5,
			Pmd:         1e-3,
			HAL:         556, // 0.3 海里
			Isolation:   Isolation{MinEpochs: 5},
			Alert:       Alert{Mode: Combined, ConfirmEpochs: 3, ClearEpochs: 3, GrossFactor: 2},
			Sources: []string{
				"ICAO Annex 10 Vol.I / PBN Manual (Doc 9613), NPA HAL=0.3 NM",
				"RTCA DO-208 (1991), §2",
				"Groves (2013), Ch.17",
			},
		},
	}
}

// Builtin 按名取内置档。
func Builtin(name string) (Profile, bool) {
	for _, p := range Builtins() {
		if p.Name == name {
			return p, true
		}
	}
	return Profile{}, false
}
