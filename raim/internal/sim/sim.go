// Package sim 为测试与回放演示提供确定性的 GNSS 仿真：
// 固定接收机真值、按方位角/仰角布置卫星、带固定随机种子的高斯噪声伪距，
// 支持给指定星注入伪距偏差与恒定钟差。
package sim

import (
	"math"
	"math/rand"

	"raim/pkg/geo"
	"raim/pkg/lsq"
)

// Receiver 是仿真接收机真值。
type Receiver struct {
	LLA       geo.LLA
	Pos       geo.Vec // ECEF 真值
	ClockBias float64 // 米
}

// NewReceiver 给定经纬度高（度/米）与钟差（米）。
func NewReceiver(lon, lat, alt, clock float64) *Receiver {
	g := geo.LLA{Lon: lon, Lat: lat, Alt: alt}
	return &Receiver{LLA: g, Pos: geo.LLAToECEF(g), ClockBias: clock}
}

// DefaultReceiver：北京附近，零钟差。
func DefaultReceiver() *Receiver { return NewReceiver(116.39, 39.90, 50, 0) }

// SatelliteDef 描述一颗几何布置的卫星。
type SatelliteDef struct {
	ID      int
	Azimuth float64 // 度（北为 0，顺时针）
	Elev    float64 // 度
	Range   float64 // 接收机到卫星距离（米），默认 21500km
}

// Constellation 是一组卫星的 ECEF 位置。
type Constellation struct {
	Rec  *Receiver
	Sats map[int]geo.Vec
}

// BuildConstellation 按方位角/仰角布星。rangeKm<=0 取 21500 km。
func BuildConstellation(rec *Receiver, defs []SatelliteDef) *Constellation {
	c := &Constellation{Rec: rec, Sats: map[int]geo.Vec{}}
	for _, d := range defs {
		az, el := d.Azimuth*geo.RadPerDeg, d.Elev*geo.RadPerDeg
		r := d.Range
		if r <= 0 {
			r = 21_500_000
		}
		// ENU 方向向量
		east := math.Cos(el) * math.Sin(az)
		north := math.Cos(el) * math.Cos(az)
		up := math.Sin(el)
		enu := geo.Vec{X: east * r, Y: north * r, Z: up * r}
		c.Sats[d.ID] = geo.Add(rec.Pos, ENUToECEF(enu, rec.LLA.Lon, rec.LLA.Lat))
	}
	return c
}

// ENUToECEF 把站心相对向量旋转回 ECEF。
func ENUToECEF(v geo.Vec, lonDeg, latDeg float64) geo.Vec {
	R := geo.ENURotation(lonDeg, latDeg)
	// ENU = R·ECEF ⇒ ECEF = Rᵀ·ENU（R 正交）
	var out geo.Vec
	out.X = R[0][0]*v.X + R[1][0]*v.Y + R[2][0]*v.Z
	out.Y = R[0][1]*v.X + R[1][1]*v.Y + R[2][1]*v.Z
	out.Z = R[0][2]*v.X + R[1][2]*v.Y + R[2][2]*v.Z
	return out
}

// EvenSky 在给定最低仰角下均布 n 颗星（低仰角星多 ⇒ 高 HDOP）。
// 同一 seed 每次结果一致。
func EvenSky(rec *Receiver, n int, minElevDeg float64, seed int64) *Constellation {
	rng := rand.New(rand.NewSource(seed))
	defs := make([]SatelliteDef, n)
	for i := 0; i < n; i++ {
		az := math.Mod(float64(i)*360.0/float64(n)+rng.Float64()*30, 360)
		el := minElevDeg + rng.Float64()*(85-minElevDeg)
		defs[i] = SatelliteDef{ID: i + 1, Azimuth: az, Elev: el}
	}
	return BuildConstellation(rec, defs)
}

// Obs 是一次带噪观测的全部参数。
type Obs struct {
	Rng    *rand.Rand
	Sigma  float64         // 所有星统一误差标准差（米）
	Bias   map[int]float64 // 给指定星注入的伪距偏差（米）
	Clock  *float64        // 若非 nil，覆盖接收机钟差（米）
	Hidden []int           // 本历元不可见的星
}

// Observe 生成一个历元的 lsq 输入。
func (c *Constellation) Observe(o Obs) *lsq.Epoch {
	sigma := o.Sigma
	if sigma <= 0 {
		sigma = 1.0
	}
	clock := c.Rec.ClockBias
	if o.Clock != nil {
		clock = *o.Clock
	}
	hidden := map[int]bool{}
	for _, id := range o.Hidden {
		hidden[id] = true
	}
	var sats []lsq.Satellite
	for id := 1; id <= len(c.Sats); id++ {
		if hidden[id] {
			continue
		}
		sp, ok := c.Sats[id]
		if !ok {
			continue
		}
		r := geo.Norm(geo.Sub(sp, c.Rec.Pos))
		pr := r + clock
		if b, ok := o.Bias[id]; ok {
			pr += b
		}
		if o.Rng != nil {
			pr += o.Rng.NormFloat64() * sigma
		}
		sats = append(sats, lsq.Satellite{ID: id, Pos: sp, PR: pr, Sigma: sigma})
	}
	return &lsq.Epoch{Approx: approxOf(c.Rec.Pos), Sats: sats}
}

// approxOf 在真值附近给一个有偏的概略位置（~3 km），模拟冷启动初值。
func approxOf(true geo.Vec) geo.Vec {
	return geo.Add(true, geo.Vec{X: 2000, Y: -1500, Z: 1000})
}

// TruePos 返回真值 ECEF（供测试比对）。
func (c *Constellation) TruePos() geo.Vec { return c.Rec.Pos }

// IDs 返回星座星编号。
func (c *Constellation) IDs() []int {
	out := make([]int, 0, len(c.Sats))
	for id := range c.Sats {
		out = append(out, id)
	}
	return out
}
