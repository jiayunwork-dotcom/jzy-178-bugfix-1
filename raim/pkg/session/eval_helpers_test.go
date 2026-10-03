package session

import (
	"math/rand"

	"raim/pkg/lsq"
	"raim/pkg/profile"
)

func testProf() profile.Profile {
	return profile.Profile{
		Name: "t", Pfa: 1e-3, Pmd: 1e-3, HAL: 1e9,
		Isolation: profile.Isolation{MinEpochs: 4},
		Alert:     profile.Alert{Mode: profile.Snapshot, ConfirmEpochs: 1, ClearEpochs: 1},
	}
}

func newSeededRand(seed int64) *rand.Rand { return rand.New(rand.NewSource(seed)) }

// EpochInputFromLSQ 把内部仿真历元转成 service 的输入（内部测试用）。
func EpochInputFromLSQ(ep *lsq.Epoch) EpochInput {
	sats := make([]SatInput, len(ep.Sats))
	for i, s := range ep.Sats {
		sats[i] = SatInput{
			ID: s.ID, Pos: [3]float64{s.Pos.X, s.Pos.Y, s.Pos.Z},
			PR: s.PR, Sigma: s.Sigma,
		}
	}
	ap := [3]float64{ep.Approx.X, ep.Approx.Y, ep.Approx.Z}
	return EpochInput{Timestamp: 1, Approx: &ap, Sats: sats}
}
