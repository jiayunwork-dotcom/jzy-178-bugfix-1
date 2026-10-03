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
	Mode       string  `json:"mode"` // ok/unavailable/detected/excluded
	SSE        float64 `json:"sse"`
	HPL        float64 `json:"hpl"`
	ExcludedID int     `json:"excluded_id,omitempty"`
	Isolated   []int   `json:"isolated"` // 本历元处于隔离（不参与解算）的星
	// IsolationChecks 给出本历元每颗“可见隔离星”的恢复评估：检验量、门限、
	// 过没过、加回后整体检验、已连续正常历元数。
	// 包含本历元新被唯一排除的星（streak=0），也包含本历元刚决定放回的星
	// （它放回前的最后一次检验，streak 已达标）；isolated 字段才是本历元
	// 解算后仍处隔离状态的权威列表。
	IsolationChecks []IsolationCheck   `json:"isolation_checks,omitempty"`
	Position        Position           `json:"position"`
	ClockBias       float64            `json:"clock_bias"`
	Iterations      int                `json:"iterations"`
	Converged       bool               `json:"converged"`
	SatResults      []detect.SatResult `json:"sat_results"`
	Trials          []detect.Trial     `json:"trials,omitempty"`

	// 跨历元状态
	Alert      bool `json:"alert"`
	BadStreak  int  `json:"bad_streak"`
	GoodStreak int  `json:"good_streak"`
	RAIMAvail  bool `json:"raim_available"`

	Reason string `json:"reason,omitempty"`
}

// IsolationCheck 是一颗隔离星在某个历元的恢复评估快照。
//
// 检验在“不含该星、也不含其他隔离星的干净基线”上完成：先用基线解预测
// 该星伪距，再比较预测值与观测值，因此其他隔离星的残差无法替它掩护。
type IsolationCheck struct {
	ID int `json:"id"`
	// TestStat 标准化预测检验量：|观测−基线预测| / 预测误差标准差。
	// 预测误差标准差按该星在基线解中的杠杆率折算（含钟差列），
	// H0 下服从标准正态。
	TestStat float64 `json:"test_stat"`
	// SelfThreshold 单星门限 √T（T 为加回解自由度 n−4 对应的卡方阈值）。
	SelfThreshold float64 `json:"self_threshold"`
	// AddSSE 把该星加回基线后的整体加权残差平方和（恒等式算得，
	// 与重解一次加回解等价，避免重复最小二乘）。
	AddSSE float64 `json:"add_sse"`
	// AddThreshold 加回解的卡方门限（自由度 n−4）。
	AddThreshold float64 `json:"add_threshold"`
	// Passed 本历元是否通过恢复检验（单星检验与加回后整体检验同时通过）。
	Passed bool `json:"passed"`
	// NormalStreak 截至本历元已连续通过的历元数（未通过/不可见为 0）。
	NormalStreak int `json:"normal_streak"`
	// Note 记录无法评估的原因（如基线不足/几何奇异）；可评估时为空。
	Note string `json:"note,omitempty"`
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
		svc.fillUnusable(sess, prof, rec, "隔离后活动星不足 4 颗，无法定位", in, visible)
	} else {
		a, err := detect.Assess(ep, detect.Options{Pfa: prof.Pfa})
		if err != nil {
			return nil, err
		}
		svc.fillFromAssessment(prof, rec, a)
		// 3) 隔离星恢复评估 + 4) 推进隔离状态（含逐个放回策略）
		newExcluded := 0
		if rec.Mode == string(detect.ModeExcluded) {
			newExcluded = rec.ExcludedID
		}
		svc.advanceIsolation(sess, prof, rec, in, visible, approx, newExcluded)
		// 5) 告警
		svc.stepAlertAndStats(sess, prof, rec)
	}

	rec.Isolated = sortedIsolated(sess.State.Isolated, visible)

	// 6) 落账
	svc.commit(sess, in.Timestamp, rec)
	return rec, nil
}

func (svc *Service) fillUnusable(sess *Session, prof profile.Profile,
	rec *EpochRecord, reason string, in EpochInput, visible map[int]bool) {
	rec.Mode = string(detect.ModeUnavailable)
	rec.RAIMAvail = false
	rec.Reason = reason
	// 活动星不足时恢复评估无法完成：所有可见隔离星本历元计不通过、计数清零。
	ids := sortedIsolated(sess.State.Isolated, visible)
	// 先让状态机推进（新排除为 0；所有隔离星给不通过评估）
	evals := make([]statem.SatEval, 0, len(ids))
	for _, id := range ids {
		evals = append(evals, statem.SatEval{ID: id, Normal: false})
	}
	sess.State.BeginEpoch(prof, 0, evals, visible, rec.Seq)
	checks := make([]IsolationCheck, 0, len(ids))
	for _, id := range ids {
		checks = append(checks, IsolationCheck{
			ID: id, Passed: false,
			NormalStreak: sess.State.Isolated[id].NormalStreak,
			Note:         reason,
		})
	}
	rec.IsolationChecks = checks
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

// isoEval 是一颗隔离星在某一干净基线上的恢复评估明细。
type isoEval struct {
	ID           int
	Normal       bool
	Z            float64 // 标准化预测检验量
	SelfThr      float64 // 单星门限 √T
	AddSSE       float64 // 加回后整体 SSE
	AddThreshold float64 // 加回解卡方门限
	Note         string
}

// advanceIsolation 完成一个历元的隔离状态推进与逐星恢复检验输出：
//
//  1. 每颗隔离星各自在“不含它、也不含其他隔离星”的干净基线上做预测检验
//     ——别的隔离星的残差不会进入它的基线，因此坏星无法互相掩护，
//     另一颗星新被唯一排除的那个历元也不会把本星已攒下的计数清零；
//  2. 计数达标（连续 MinEpochs 个正常历元）的星按“逐个放回、放回后
//     整体复核”的策略处理：每放回一颗，就把其余候选在扩大后的基线上
//     重新评估，联合复核不过则该候选保持隔离、计数清零。
func (svc *Service) advanceIsolation(sess *Session, prof profile.Profile,
	rec *EpochRecord, in EpochInput, visible map[int]bool, approx geo.Vec, excludedNow int) {

	// 干净基线：本历元可见且未被隔离的活动星（排除成功时该星不在活动集里，
	// 因为活动星构造时它尚未入隔离，因此需显式剔除新排除星）。
	baseSats := make([]lsq.Satellite, 0, len(in.Sats))
	for _, s := range in.Sats {
		if s.ID == excludedNow {
			continue
		}
		if sess.State.Isolated[s.ID] != nil {
			continue
		}
		baseSats = append(baseSats, toLSQSat(s))
	}

	// 此前已隔离且可见的星（新排除星稍后单独评估，不能作为“已连续正常”计入）
	priorIDs := make([]int, 0, len(sess.State.Isolated))
	for id := range sess.State.Isolated {
		if visible[id] && id != excludedNow {
			priorIDs = append(priorIDs, id)
		}
	}
	sort.Ints(priorIDs)

	priorEval := make(map[int]*isoEval, len(priorIDs))
	evals := make([]statem.SatEval, 0, len(priorIDs))
	for _, id := range priorIDs {
		e := evalOne(prof, in, baseSats, approx, id)
		priorEval[id] = e
		evals = append(evals, statem.SatEval{ID: id, Normal: e.Normal, Z: e.Z})
	}

	// 计数推进（不立即放回）
	sess.State.BeginEpoch(prof, excludedNow, evals, visible, rec.Seq)

	// 本历元新被唯一排除的星：也给出它这一历元的检验量（在干净基线上），
	// 便于回放观察“它为什么进隔离”。它的恢复计数从 0 开始。
	var newEval *isoEval
	if excludedNow != 0 && visible[excludedNow] {
		e := evalOne(prof, in, baseSats, approx, excludedNow)
		newEval = e
	}

	// 逐个放回 + 放回后整体复核
	released := map[int]bool{}
	chainBase := baseSats
	for {
		cand := sess.State.Qualifying(prof)
		if len(cand) == 0 {
			break
		}
		// 每次只放一颗：在当前基线上检验量最小（最正常）的先放，
		// 次序确定、不依赖 map 遍历。
		best := cand[0]
		bestZ := math.Inf(1)
		for _, id := range cand {
			if e := priorEval[id]; e != nil && e.Normal && math.Abs(e.Z) < bestZ {
				bestZ, best = math.Abs(e.Z), id
			}
		}
		sess.State.Release(best)
		released[best] = true
		if len(cand) == 1 {
			break
		}
		// 扩大基线并对剩余候选整体复核
		if t, ok := findSat(in.Sats, best); ok {
			chainBase = append(chainBase, toLSQSat(t))
		}
		for _, id := range cand {
			if id == best {
				continue
			}
			re := evalOne(prof, in, chainBase, approx, id)
			priorEval[id] = re
			if !re.Normal {
				// 联合复核不过：保持隔离、连续计数清零
				sess.State.ResetStreak(id)
			}
		}
	}

	// 组装本历元逐星检验快照（按星编号排序；不可见隔离星不出现在输出中）
	checkIDs := append(append([]int{}, priorIDs...), excludedIfVisible(excludedNow, visible)...)
	rec.IsolationChecks = make([]IsolationCheck, 0, len(checkIDs))
	for _, id := range checkIDs {
		if id == 0 {
			continue
		}
		var e *isoEval
		if id == excludedNow {
			e = newEval
		} else {
			e = priorEval[id]
		}
		if e == nil {
			// 本历元不可见（只可能是已放回的候选）：快照按不可见处理，不输出。
			continue
		}
		streak := 0
		if en := sess.State.Isolated[id]; en != nil {
			streak = en.NormalStreak
		} else if released[id] {
			streak = prof.Isolation.MinEpochs // 本历元放回：计数已达标
		}
		rec.IsolationChecks = append(rec.IsolationChecks, IsolationCheck{
			ID: id, TestStat: math.Abs(e.Z), SelfThreshold: e.SelfThr,
			AddSSE: e.AddSSE, AddThreshold: e.AddThreshold,
			Passed: e.Normal, NormalStreak: streak, Note: e.Note,
		})
	}
}

// evalOne 在给定干净基线上评估一颗星的恢复资格：
//
//   - 基线（不含该星）最小二乘解；
//   - 用基线解预测该星伪距，预测残差按其杠杆率折算成标准化检验量 z，
//     门限取加回解卡方门限的平方根 √T（H0 下 z 服从标准正态）；
//   - 加回该星后的整体 SSE 由恒等式 SSE_add = SSE_base + z² 给出，
//     与重解一次加回解等价（线性模型恒等），门限为自由度 n−4 的卡方门限；
//   - 两个条件同时满足才算通过。
func evalOne(prof profile.Profile, in EpochInput, baseSats []lsq.Satellite,
	approx geo.Vec, targetID int) *isoEval {
	e := &isoEval{ID: targetID}
	if len(baseSats) < 4 {
		e.Note = "基线不足 4 颗，无法评估"
		return e
	}
	base, err := lsq.Solve(&lsq.Epoch{Approx: approx, Sats: baseSats})
	if err != nil {
		e.Note = "基线解算失败（几何奇异）"
		return e
	}
	t, ok := findSat(in.Sats, targetID)
	if !ok {
		e.Note = "目标星本历元不可见"
		return e
	}
	// 基线解位置下该星的视线行 g=[-ux -uy -uz 1] 与无钟差预测距离
	tPos := geo.Vec{X: t.Pos[0], Y: t.Pos[1], Z: t.Pos[2]}
	d := geo.Sub(tPos, base.Pos)
	rng := geo.Norm(d)
	ux, uy, uz := d.X/rng, d.Y/rng, d.Z/rng
	g := [4]float64{-ux, -uy, -uz, 1}
	// 预测残差 δ = 观测伪距 − (几何距离 + 基线钟差)
	delta := t.PR - (rng + base.ClockBias)
	// 预测误差方差（米²）：v = σ² + gᵀ(GᵀWG)^{-1}g。
	// 各星 σ 相等时 (GᵀWG)^{-1}=σ²(GᵀG)^{-1}，即 v=σ²(1+h)；
	// 这里用加权一般形式，兼容混合 σ。
	inv := base.NormalInv()
	q := 0.0
	for i := 0; i < 4; i++ {
		for j := 0; j < 4; j++ {
			q += g[i] * inv[i][j] * g[j]
		}
	}
	predVar := t.Sigma*t.Sigma + q
	z := delta / math.Sqrt(math.Max(predVar, 1e-30))

	baseSSE, _ := lsq.WeightedSSE(base.Resid, base.Sigma)
	nAdd := len(baseSats) + 1
	dofAdd := nAdd - 4
	if dofAdd <= 0 {
		e.Note = "加回后无冗余"
		return e
	}
	thr := chisq.Threshold(dofAdd, prof.Pfa)
	// 加回该星后的整体 SSE 由加权最小二乘的恒等式给出：
	// SSE_add = SSE_base + δ²/v = SSE_base + z²，
	// 与重解一次“基线+该星”的加回解得到的 SSE 相同（线性模型精确成立）。
	addSSE := baseSSE + z*z
	e.Z = z
	e.SelfThr = math.Sqrt(thr)
	e.AddSSE = addSSE
	e.AddThreshold = thr
	e.Normal = math.Abs(z) <= math.Sqrt(thr) && addSSE <= thr
	return e
}

func findSat(sats []SatInput, id int) (SatInput, bool) {
	for _, s := range sats {
		if s.ID == id {
			return s, true
		}
	}
	return SatInput{}, false
}

func excludedIfVisible(id int, visible map[int]bool) []int {
	if id != 0 && visible[id] {
		return []int{id}
	}
	return nil
}

func toLSQSat(s SatInput) lsq.Satellite {
	return lsq.Satellite{
		ID:    s.ID,
		Pos:   geo.Vec{X: s.Pos[0], Y: s.Pos[1], Z: s.Pos[2]},
		PR:    s.PR,
		Sigma: s.Sigma,
	}
}
