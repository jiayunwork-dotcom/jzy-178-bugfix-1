// Command replaydemo 用确定性仿真的“录好飞行数据”回放三种告警判定方式：
//
//	snapshot    逐历元快照判定
//	persistence 持续性窗口判定
//	combined    组合：窗口 + 恶劣超限立即告警
//
// 输出每种方式在相同数据上的首次告警延迟、告警/误警次数与可用率，
// 供 docs/design.md 引用。该演示只用纯计算（service），不经过 HTTP/磁盘。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"math/rand"
	"os"

	"raim/internal/sim"
	"raim/pkg/profile"
	"raim/pkg/session"
)

func main() {
	seed := flag.Int64("seed", 20260930, "固定随机种子")
	flag.Parse()

	modeProfiles := map[string]profile.Profile{}
	base := func(name string, mode profile.AlertMode, gross float64) profile.Profile {
		return profile.Profile{
			Name: name, Pfa: 1e-3, Pmd: 1e-3, HAL: 1e9, // 本演示只看检测告警，不引入 HPL 告警
			Isolation: profile.Isolation{MinEpochs: 5},
			Alert:     profile.Alert{Mode: mode, ConfirmEpochs: 3, ClearEpochs: 3, GrossFactor: gross},
		}
	}
	modeProfiles["snapshot"] = base("snapshot", profile.Snapshot, 0)
	modeProfiles["persistence"] = base("persistence", profile.Persistence, 0)
	// 演示把“恶劣”门限取 3×，让“可检出但不恶劣”的中等故障落在 1×~3× 区间
	modeProfiles["combined"] = base("combined", profile.Combined, 3)

	// 8 颗星：自由度 4，SSE 分布较集中；演示关注三种告警判定的差异。
	rec := sim.DefaultReceiver()
	c := sim.EvenSky(rec, 8, 20, *seed)

	type scenario struct {
		name      string
		n         int
		biasAt    func(seq0 int) map[int]float64
		faultFrom int // 真实持续故障窗口 [faultFrom, faultTo]（0-based，含端点）
		faultTo   int
	}
	const noFault = -1
	scenarios := []scenario{
		{
			// 基线：全程无故障（验证可用率 100%、零告警）
			name:      "clean_baseline",
			n:         1000,
			biasAt:    func(i int) map[int]float64 { return nil },
			faultFrom: noFault, faultTo: noFault,
		},
		{
			// 只有单历元“尖峰”（残差尖峰/多路径），无持续故障：
			// 快照会逐次拉起又撤下 -> 闪烁告警（按真值都算误警）；
			// 持续性/组合窗口能滤掉。
			name:      "transient_spikes",
			n:         1000,
			faultFrom: noFault, faultTo: noFault,
			biasAt: func(i int) map[int]float64 {
				if i == 120 || i == 480 || i == 760 {
					return map[int]float64{4: 7, 5: 7}
				}
				return nil
			},
		},
		{
			// 一段持续的可检出但不恶劣故障（双星中等偏差，无法单星排除）
			name: "persistent_fault", n: 60, faultFrom: 20, faultTo: 40,
			biasAt: func(i int) map[int]float64 {
				if i >= 20 && i <= 40 {
					return map[int]float64{4: 7, 5: 7}
				}
				return nil
			},
		},
		{
			// 一段恶劣故障（双星大偏差，无法单星排除，SSE ≥3x 阈值；
			// 组合模式应立即告警，持续性仍要等满窗口）
			name: "gross_fault", n: 40, faultFrom: 15, faultTo: 25,
			biasAt: func(i int) map[int]float64 {
				if i >= 15 && i <= 25 {
					return map[int]float64{4: 20, 5: 20}
				}
				return nil
			},
		},
	}

	out := map[string]map[string]any{}
	for _, sc := range scenarios {
		epochs := gen(c, sc.n, *seed, sc.biasAt)
		res := map[string]any{}
		for name, p := range modeProfiles {
			svc := session.NewService([]profile.Profile{p})
			sess, _ := svc.NewSession("demo", name)
			alerts := make([]bool, sc.n)
			firstAlert := noFault
			for i, e := range epochs {
				r, err := svc.Step(sess, e)
				if err != nil {
					panic(err)
				}
				alerts[i] = r.Alert
				if firstAlert == noFault && r.Alert {
					firstAlert = i
				}
			}

			episodes, trueEpisodes, falseEpisodes := classifyEpisodes(alerts, sc.faultFrom, sc.faultTo)

			relDelay := noFault
			if firstAlert != noFault && sc.faultFrom != noFault {
				relDelay = firstAlert - sc.faultFrom + 1
			}
			alertEpochs := 0
			for _, a := range alerts {
				if a {
					alertEpochs++
				}
			}
			res[name] = map[string]any{
				"first_alert_delay_epochs": relDelay,
				"alert_episodes":           episodes,
				"true_alarm_episodes":      trueEpisodes,
				"false_alarm_episodes":     falseEpisodes,
				"alert_epochs":             alertEpochs,
				"service_availability":     1 - float64(alertEpochs)/float64(sc.n),
				"raim_availability":        sess.Stats.RAIMAvailRate,
			}
		}
		out[sc.name] = res
	}

	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	if err := enc.Encode(out); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// classifyEpisodes 把逐历元告警序列切成连续片段，并按“片段是否覆盖真实
// 持续故障窗口”区分真告警与误警（尖峰不在故障窗口内，全部算误警）。
// faultFrom=-1 表示该场景没有真实持续故障。
func classifyEpisodes(alerts []bool, faultFrom, faultTo int) (total, trueN, falseN int) {
	in := false
	epFrom, epTo := 0, 0
	flush := func() {
		total++
		overlaps := faultFrom >= 0 && epTo >= faultFrom && epFrom <= faultTo
		if overlaps {
			trueN++
		} else {
			falseN++
		}
	}
	for i, a := range alerts {
		if a && !in {
			in, epFrom, epTo = true, i, i
		} else if a && in {
			epTo = i
		} else if !a && in {
			flush()
			in = false
		}
	}
	if in {
		flush()
	}
	return
}

func gen(c *sim.Constellation, n int, seed int64, biasAt func(int) map[int]float64) []session.EpochInput {
	rng := rand.New(rand.NewSource(seed))
	out := make([]session.EpochInput, n)
	for i := range out {
		ep := c.Observe(sim.Obs{Rng: rng, Sigma: 1, Bias: biasAt(i)})
		sats := make([]session.SatInput, len(ep.Sats))
		for j, s := range ep.Sats {
			sats[j] = session.SatInput{ID: s.ID, Pos: [3]float64{s.Pos.X, s.Pos.Y, s.Pos.Z}, PR: s.PR, Sigma: s.Sigma}
		}
		out[i] = session.EpochInput{Timestamp: int64(i + 1), Sats: sats}
	}
	return out
}
