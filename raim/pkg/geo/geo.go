// Package geo 实现 WGS84 坐标换算：ECEF 直角坐标与大地坐标（经度、纬度、椭球高）互转，
// 以及 ENU 旋转矩阵（保护级需要水平投影）。
package geo

import "math"

// WGS84 椭球参数。
const (
	A         = 6378137.0           // 长半轴 a，单位 m
	F         = 1.0 / 298.257223563 // 扁率
	B         = A * (1.0 - F)       // 短半轴
	E2        = F * (2.0 - F)       // 第一偏心率平方
	Ep2       = E2 / (1.0 - E2)     // 第二偏心率平方
	RadPerDeg = math.Pi / 180.0
	DegPerRad = 180.0 / math.Pi
)

// LLA 为大地坐标：经度、纬度（度）与椭球高（米）。
type LLA struct {
	Lon float64 `json:"lon"`
	Lat float64 `json:"lat"`
	Alt float64 `json:"alt"`
}

// Vec 是 ECEF 三维向量。
type Vec struct {
	X float64 `json:"x"`
	Y float64 `json:"y"`
	Z float64 `json:"z"`
}

// Sub、Add、Scale、Norm 为 ECEF 向量基础运算。
func Sub(p, q Vec) Vec           { return Vec{p.X - q.X, p.Y - q.Y, p.Z - q.Z} }
func Add(p, q Vec) Vec           { return Vec{p.X + q.X, p.Y + q.Y, p.Z + q.Z} }
func Scale(p Vec, k float64) Vec { return Vec{p.X * k, p.Y * k, p.Z * k} }
func Norm(p Vec) float64         { return math.Sqrt(p.X*p.X + p.Y*p.Y + p.Z*p.Z) }
func Dot(p, q Vec) float64       { return p.X*q.X + p.Y*q.Y + p.Z*q.Z }

// ECEFToLLA 按 Bowring 闭式迭代把 ECEF 换算为经纬度与椭球高。
// 迭代一般 2~3 次即收敛到机器精度。
func ECEFToLLA(p Vec) LLA {
	x, y, z := p.X, p.Y, p.Z
	lon := math.Atan2(y, x)
	p2 := math.Sqrt(x*x + y*y)

	if p2 < 1.0 {
		// 近地轴：直接按极点处理。
		h := math.Abs(z) - B
		lat := math.Copysign(90.0, z)
		return LLA{Lon: 0, Lat: lat, Alt: h}
	}

	theta := math.Atan2(z*A, p2*B)
	lat := math.Atan2(
		z+Ep2*B*math.Pow(math.Sin(theta), 3),
		p2-E2*A*math.Pow(math.Cos(theta), 3),
	)
	var n float64
	for i := 0; i < 5; i++ {
		sinLat := math.Sin(lat)
		n = A / math.Sqrt(1.0-E2*sinLat*sinLat)
		next := math.Atan2(z+E2*n*sinLat, p2)
		if math.Abs(next-lat) < 1e-13 {
			lat = next
			break
		}
		lat = next
	}
	sinLat := math.Sin(lat)
	n = A / math.Sqrt(1.0-E2*sinLat*sinLat)
	alt := p2/math.Cos(lat) - n

	return LLA{Lon: lon * DegPerRad, Lat: lat * DegPerRad, Alt: alt}
}

// LLAToECEF 把经纬度（度）与椭球高换算为 ECEF。
func LLAToECEF(g LLA) Vec {
	lat, lon := g.Lat*RadPerDeg, g.Lon*RadPerDeg
	sinLat, cosLat := math.Sin(lat), math.Cos(lat)
	n := A / math.Sqrt(1.0-E2*sinLat*sinLat)
	return Vec{
		X: (n + g.Alt) * cosLat * math.Cos(lon),
		Y: (n + g.Alt) * cosLat * math.Sin(lon),
		Z: (n*(1.0-E2) + g.Alt) * sinLat,
	}
}

// ENURotation 返回把 ECEF 相对向量转到以 lon/lat（度）为原点的
// 站心坐标系（东、北、天）的 3x3 矩阵（按行存储）。
func ENURotation(lonDeg, latDeg float64) [3][3]float64 {
	lat, lon := latDeg*RadPerDeg, lonDeg*RadPerDeg
	sinLon, cosLon := math.Sin(lon), math.Cos(lon)
	sinLat, cosLat := math.Sin(lat), math.Cos(lat)
	return [3][3]float64{
		{-sinLon, cosLon, 0},
		{-sinLat * cosLon, -sinLat * sinLon, cosLat},
		{cosLat * cosLon, cosLat * sinLon, sinLat},
	}
}
