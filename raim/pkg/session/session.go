// Package session 实现回放会话：绑定运行档、逐历元（或批量）提交，
// 编排“单历元定位/检测/排除/保护级”与“跨历元隔离、告警状态机”，
// 并做时间戳去重/倒退校验与统计。
//
// 同一段数据无论一次性批量提交还是逐历元实时提交，都走同一个 Step，
// 因此逐历元结果严格一致；状态持久化后重启续跑同样复用该 Step。
package session

import (
	"fmt"
	"math"
	"sort"
	"time"

	"raim/pkg/apierr"
	"raim/pkg/chisq"
	"raim/pkg/detect"
	"raim/pkg/geo"
	"raim/pkg/lsq"
	"raim/pkg/profile"
	"raim/pkg/protect"
	"raim/pkg/statem"
)

// sortedIsolated 返回当前隔离且本历元可见的星编号（升序）。
func sortedIsolated(isolated map[int]*statem.IsolationEntry, visible map[int]bool) []int {
	var ids []int
	for id := range isolated {
		if visible[id] {
			ids = append(ids, id)
		}
	}
	sort.Ints(ids)
	return ids
}

// SatInput 是调用方提交的单星观测。
type SatInput struct {
	ID    int        `json:"id"`
	Pos   [3]float64 `json:"pos"` // ECEF x,y,z
	PR    float64    `json:"pr"`
	Sigma float64    `json:"sigma"`
}

// EpochInput 是一个历元的提交内容。Approx 为可选概略位置（首历元建议提供）。
type EpochInput struct {
	Timestamp int64       `json:"timestamp"` // 单调时间戳（任意单位，如 Unix 秒）
	Approx    *[3]float64 `json:"approx,omitempty"`
	Sats      []SatInput  `json:"satellites"`
}

// EpochRecord 是一个历元的完整判定快照（逐历元结果）。
type EpochRecord struct {
	Seq       int   `json:"seq"`
	Timestamp int64 `json:"timestamp"`

	// 所用档与当时阈值（每个历元都注明）
	ProfileName string  `json:"profile_name"`
	Pfa         float64 `json:"pfa"`
	Pmd         float64 `json:"pmd"`
	HAL         float64 `json:"hal"`
	Threshold   float64 `json:"chi_square_threshold"`
	DOF         int     `json:"dof"`

	// 单历元结果
	Mode       string             `json:"mode"` // ok/unavailable/detected/excluded
	SSE        float64            `json:"sse"`
	HPL        float64            `json:"hpl"`
	ExcludedID int                `json:"excluded_id,omitempty"`
	Isolated   []int              `json:"isolated"` // 本历元处于隔离（不参与解算）的星
	Position   Position           `json:"position"`
	ClockBias  float64            `json:"clock_bias"`
	Iterations int                `json:"iterations"`
	Converged  bool               `json:"converged"`
	SatResults []detect.SatResult `json:"sat_results"`
	Trials     []detect.Trial     `json:"trials,omitempty"`

	// 跨历元状态
	Alert      bool `json:"alert"`
	BadStreak  int  `json:"bad_streak"`
	GoodStreak int  `json:"good_streak"`
	RAIMAvail  bool `json:"raim_available"`

	Reason string `json:"reason,omitempty"`
}

// Position 同时给出 ECEF 与经纬度高。
type Position struct {
	ECEF [3]float64 `json:"ecef"`
	Lon  float64    `json:"lon"`
	Lat  float64    `json:"lat"`
	Alt  float64    `json:"alt"`
}

// Stats 是会话级统计。
type Stats struct {
	Epochs        int     `json:"epochs"`
	RAIMAvailable int     `json:"raim_available_epochs"`
	Alerts        int     `json:"alert_epochs"`
	Detections    int     `json:"detections"`           // 检出故障的历元数
	AlertEpisodes int     `json:"alert_episodes"`       // 告警片段数
	FalseAlarms   int     `json:"false_alarm_episodes"` // 未识别出故障星的告警片段数
	RAIMAvailRate float64 `json:"raim_availability"`    // RAIM 可用历元占比
	ServiceAvail  float64 `json:"service_availability"` // 未告警历元占比
}

// Session 是一个回放会话的全部持久内容。
type Session struct {
	ID          string         `json:"id"`
	ProfileName string         `json:"profile_name"`
	CreatedAt   time.Time      `json:"created_at"`
	LastTS      *int64         `json:"last_ts"`
	State       statem.State   `json:"state"`
	Stats       Stats          `json:"stats"`
	Records     []*EpochRecord `json:"records"`

	// 当前告警片段内是否已识别出真实风险（误警记账用，需持久化）
	AlertOpenRisk bool `json:"alert_open_risk"`
}

// Service 在无存储依赖的情况下驱动会话计算（Store 负责持久化）。
type Service struct {
	profiles map[string]profile.Profile
}

// NewService 用给定运行档集合构造服务。
func NewService(ps []profile.Profile) *Service {
	m := map[string]profile.Profile{}
	for _, p := range ps {
		m[p.Name] = p
	}
	return &Service{profiles: m}
}

// Profile 返回已注册运行档。
func (svc *Service) Profile(name string) (profile.Profile, bool) {
	p, ok := svc.profiles[name]
	return p, ok
}

// NewSession 创建绑定运行档的会话。
func (svc *Service) NewSession(id, profileName string) (*Session, error) {
	if _, ok := svc.profiles[profileName]; !ok {
		return nil, apierr.ErrProfileNotFound
	}
	return &Session{
		ID:          id,
		ProfileName: profileName,
		CreatedAt:   time.Now().UTC(),
		State:       statem.NewState(),
	}, nil
}

// Step 处理一个历元并返回该历元记录。重复/倒退时间戳分别返回哨兵错误，不推进状态。
func (svc *Service) Step(sess *Session, in EpochInput) (*EpochRecord, error) {
	prof, ok := svc.profiles[sess.ProfileName]
	if !ok {
		return nil, apierr.ErrProfileNotFound
	}
	if sess.LastTS != nil {
		if in.Timestamp == *sess.LastTS {
			return nil, apierr.ErrDuplicateEpoch
		}
		if in.Timestamp < *sess.LastTS {
			return nil, apierr.ErrStaleEpoch
		}
	}
	if len(in.Sats) < 4 {
		return nil, apierr.New("satellites",
			fmt.Sprintf("历元 %d 可见星 %d 颗，不足 4 颗", in.Timestamp, len(in.Sats)))
	}

	visible := map[int]bool{}
	for _, s := range in.Sats {
		visible[s.ID] = true
	}
	approx := svc.approxFor(sess, in)

	rec := &EpochRecord{
		Seq:         sess.Stats.Epochs + 1,
		Timestamp:   in.Timestamp,
		ProfileName: prof.Name,
		Pfa:         prof.Pfa,
		Pmd:         prof.Pmd,
		HAL:         prof.HAL,
	}

	// 1) 活动星（剔除隔离星）
	var active []lsq.Satellite
	for _, s := range in.Sats {
		if sess.State.Isolated[s.ID] != nil {
			continue
		}
		active = append(active, toLSQSat(s))
	}

	// 2) 单历元检测/排除
	ep := &lsq.Epoch{Approx: approx, Sats: active}
	if len(active) < 4 {
		svc.fillUnusable(sess, prof, rec, "隔离后活动星不足 4 颗，无法定位", visible)
	} else {
		a, err := detect.Assess(ep, detect.Options{Pfa: prof.Pfa})
		if err != nil {
			return nil, err
		}
		svc.fillFromAssessment(prof, rec, a)
		// 3) 隔离星恢复评估（隔离期间仍逐历元算检验量）
		recovery := svc.evalRecovery(prof, in, sess.State.Isolated, approx)
		// 4) 推进隔离状态
		newExcluded := 0
		if rec.Mode == string(detect.ModeExcluded) {
			newExcluded = rec.ExcludedID
		}
		sess.State.UpdateIsolation(prof, newExcluded, recovery, visible, rec.Seq)
		// 5) 告警
		svc.stepAlertAndStats(sess, prof, rec)
	}

	rec.Isolated = sortedIsolated(sess.State.Isolated, visible)

	// 6) 落账
	svc.commit(sess, in.Timestamp, rec)
	return rec, nil
}

func (svc *Service) fillUnusable(sess *Session, prof profile.Profile,
	rec *EpochRecord, reason string, visible map[int]bool) {
	rec.Mode = string(detect.ModeUnavailable)
	rec.RAIMAvail = false
	rec.Reason = reason
	// 活动星不足时隔离计数仍按“不可见/无法评估”处理：可见性以原观测为准，
	// 但恢复评估依赖定位，无法完成，故所有隔离星 streak 清零（Normal=false）。
	var recovery []statem.SatRecovery
	for id := range sess.State.Isolated {
		if visible[id] {
			recovery = append(recovery, statem.SatRecovery{ID: id, Normal: false})
		}
	}
	sess.State.UpdateIsolation(prof, 0, recovery, visible, rec.Seq)
	svc.stepAlert(sess, prof, rec, statem.EpochStatus{
		IntegrityAvailable: false, IntegrityBad: true,
	})
}

func (svc *Service) fillFromAssessment(prof profile.Profile,
	rec *EpochRecord, a *detect.Assessment) {
	rec.Mode = string(a.Mode)
	rec.SSE = a.SSE
	rec.DOF = a.DOF
	rec.Threshold = a.Threshold
	rec.SatResults = a.SatResults
	rec.Trials = a.Trials
	rec.Reason = a.Reason
	rec.ExcludedID = a.ExcludedID
	if a.Sol != nil {
		rec.Position = Position{
			ECEF: [3]float64{a.Sol.Pos.X, a.Sol.Pos.Y, a.Sol.Pos.Z},
			Lon:  a.Sol.LLA.Lon, Lat: a.Sol.LLA.Lat, Alt: a.Sol.LLA.Alt,
		}
		rec.ClockBias = a.Sol.ClockBias
		rec.Iterations = a.Sol.Iter
		rec.Converged = a.Sol.Converged
	}
	rec.RAIMAvail = a.Used >= 5
	if rec.RAIMAvail {
		rec.HPL = protect.AssessmentHPL(a, prof.Pmd)
	}
}

func (svc *Service) stepAlertAndStats(sess *Session, prof profile.Profile, rec *EpochRecord) {
	bad, gross := false, false
	switch rec.Mode {
	case string(detect.ModeDetected):
		bad = true
	case string(detect.ModeUnavailable):
		bad = true
	}
	if rec.RAIMAvail && rec.HPL > prof.HAL {
		bad = true
		if prof.Alert.Mode == profile.Combined && rec.HPL >= prof.Alert.GrossFactor*prof.HAL {
			gross = true
		}
	}
	if rec.Mode == string(detect.ModeDetected) && rec.Threshold > 0 &&
		prof.Alert.Mode == profile.Combined &&
		rec.SSE >= prof.Alert.GrossFactor*rec.Threshold {
		gross = true
	}
	svc.stepAlert(sess, prof, rec, statem.EpochStatus{
		IntegrityAvailable: rec.RAIMAvail,
		IntegrityBad:       bad,
		IdentifiedRisk: rec.Mode == string(detect.ModeDetected) ||
			rec.Mode == string(detect.ModeExcluded),
		Gross: gross,
	})
}

func (svc *Service) stepAlert(sess *Session, prof profile.Profile,
	rec *EpochRecord, st statem.EpochStatus) {
	wasActive := sess.State.Alert.Active
	active := sess.State.StepAlert(prof, st)
	rec.Alert = active
	rec.BadStreak = sess.State.Alert.BadStreak
	rec.GoodStreak = sess.State.Alert.GoodStreak

	if active && !wasActive {
		sess.Stats.AlertEpisodes++
		sess.AlertOpenRisk = false
	}
	if active && st.IdentifiedRisk {
		// 片段内任一年元识别出真实风险（检出故障），该片段不算误警
		sess.AlertOpenRisk = true
	}
	if !active && wasActive && !sess.AlertOpenRisk {
		sess.Stats.FalseAlarms++
	}
}

func (svc *Service) commit(sess *Session, ts int64, rec *EpochRecord) {
	sess.LastTS = &ts
	sess.Stats.Epochs++
	if rec.RAIMAvail {
		sess.Stats.RAIMAvailable++
	}
	if rec.Mode == string(detect.ModeDetected) || rec.Mode == string(detect.ModeExcluded) {
		sess.Stats.Detections++
	}
	if rec.Alert {
		sess.Stats.Alerts++
	}
	sess.Records = append(sess.Records, rec)
	if n := sess.Stats.Epochs; n > 0 {
		sess.Stats.RAIMAvailRate = float64(sess.Stats.RAIMAvailable) / float64(n)
		sess.Stats.ServiceAvail = float64(n-sess.Stats.Alerts) / float64(n)
	}
}

// approxFor 决定本历元迭代初值：显式提供 > 上一历元定位 > 首历元卫星反推。
func (svc *Service) approxFor(sess *Session, in EpochInput) geo.Vec {
	if in.Approx != nil {
		return geo.Vec{X: in.Approx[0], Y: in.Approx[1], Z: in.Approx[2]}
	}
	if n := len(sess.Records); n > 0 {
		p := sess.Records[n-1].Position.ECEF
		return geo.Vec{X: p[0], Y: p[1], Z: p[2]}
	}
	return firstSatApprox(in)
}

// firstSatApprox 在首历元未提供初值时，由卫星位置反推一个“指向该星的地表点”
// （单位矢量缩到 WGS84 长半轴）。距接收机通常在数百 km 内，足以让线性化收敛。
func firstSatApprox(in EpochInput) geo.Vec {
	for _, s := range in.Sats {
		p := geo.Vec{X: s.Pos[0], Y: s.Pos[1], Z: s.Pos[2]}
		r := geo.Norm(p)
		if r > 1 {
			return geo.Scale(p, geo.A/r)
		}
	}
	return geo.Vec{X: geo.A, Y: 0, Z: 0}
}

// evalRecovery 对每个“当前隔离且本历元可见”的星做恢复评估：
//   - 本星在含自身的全解中标准化残差正常；
//   - 把它加回去后整体检验通过（SSE ≤ 阈值）。
//
// 两者同时满足才算一个恢复历元。
func (svc *Service) evalRecovery(prof profile.Profile, in EpochInput,
	isolated map[int]*statem.IsolationEntry, approx geo.Vec) []statem.SatRecovery {
	if len(isolated) == 0 {
		return nil
	}
	all := make([]lsq.Satellite, 0, len(in.Sats))
	byID := map[int]lsq.Satellite{}
	for _, s := range in.Sats {
		l := toLSQSat(s)
		all = append(all, l)
		byID[s.ID] = l
	}
	var out []statem.SatRecovery
	for id := range isolated {
		if _, ok := byID[id]; !ok {
			continue // 不可见：上层 UpdateIsolation 会清零
		}
		sol, err := lsq.Solve(&lsq.Epoch{Approx: approx, Sats: all})
		if err != nil {
			out = append(out, statem.SatRecovery{ID: id, Normal: false})
			continue
		}
		sse, dof := lsq.WeightedSSE(sol.Resid, sol.Sigma)
		overallPass := false
		selfZ := 0.0
		if dof > 0 {
			thr := chisq.Threshold(dof, prof.Pfa)
			overallPass = sse <= thr
			for i, s := range all {
				if s.ID == id {
					selfZ = math.Abs(sol.Resid[i]) / s.Sigma
					break
				}
			}
			// 单星正常门限取 √thr（与整体卡方阈值一致的保守判据）
			if overallPass && selfZ <= math.Sqrt(thr) {
				out = append(out, statem.SatRecovery{ID: id, Normal: true})
				continue
			}
		}
		out = append(out, statem.SatRecovery{ID: id, Normal: false})
	}
	return out
}

func toLSQSat(s SatInput) lsq.Satellite {
	return lsq.Satellite{
		ID:    s.ID,
		Pos:   geo.Vec{X: s.Pos[0], Y: s.Pos[1], Z: s.Pos[2]},
		PR:    s.PR,
		Sigma: s.Sigma,
	}
}
