package geo

import "testing"

func TestECEFLLARoundTrip(t *testing.T) {
	cases := []LLA{
		{Lon: 116.391, Lat: 39.907, Alt: 50},
		{Lon: -74.006, Lat: 40.713, Alt: 10},
		{Lon: 0, Lat: 0, Alt: 0},
		{Lon: 12.5, Lat: -33.9, Alt: 1500},
		{Lon: 180, Lat: -89.999, Alt: 0},
	}
	for _, g := range cases {
		p := LLAToECEF(g)
		back := ECEFToLLA(p)
		if d := (back.Lon-g.Lon)*(back.Lon-g.Lon) + (back.Lat-g.Lat)*(back.Lat-g.Lat); d > 1e-12 {
			t.Errorf("经纬度往返偏差过大: %+v -> %+v", g, back)
		}
		if abs(back.Alt-g.Alt) > 1e-6 {
			t.Errorf("高度往返偏差 %v m: %+v -> %+v", back.Alt-g.Alt, g, back)
		}
	}
}

func TestKnownSurveyPoint(t *testing.T) {
	// 格林尼治子午线、赤道、海平面附近点 ECEF 约 (a,0,0)
	p := LLAToECEF(LLA{Lon: 0, Lat: 0, Alt: 0})
	if abs(p.X-A) > 1e-6 || abs(p.Y) > 1e-6 || abs(p.Z) > 1e-6 {
		t.Fatalf("赤道零经度点 = %+v", p)
	}
}

func TestENURotationOrthogonal(t *testing.T) {
	R := ENURotation(116.3, 39.9)
	// R Rᵀ = I
	for i := 0; i < 3; i++ {
		for j := 0; j < 3; j++ {
			dot := 0.0
			for k := 0; k < 3; k++ {
				dot += R[i][k] * R[j][k]
			}
			want := 0.0
			if i == j {
				want = 1
			}
			if abs(dot-want) > 1e-12 {
				t.Fatalf("ENU 矩阵非正交 [%d][%d]=%v", i, j, dot)
			}
		}
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
