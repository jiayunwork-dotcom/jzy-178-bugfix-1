package profile_test

import (
	"testing"

	"raim/pkg/apierr"
	"raim/pkg/profile"
)

func TestBuiltins(t *testing.T) {
	names := map[string]bool{"enroute": false, "terminal": false, "npa": false}
	var hals []float64
	for _, p := range profile.Builtins() {
		if err := p.Validate(); err != nil {
			t.Fatalf("内置档 %s 校验失败: %v", p.Name, err)
		}
		if len(p.Sources) == 0 {
			t.Fatalf("内置档 %s 缺少出处", p.Name)
		}
		names[p.Name] = true
		hals = append(hals, p.HAL)
	}
	for n, ok := range names {
		if !ok {
			t.Fatalf("缺少内置档 %s", n)
		}
	}
	// HAL 应随飞行阶段收紧
	if !(hals[0] > hals[1] && hals[1] > hals[2]) {
		t.Fatalf("HAL 应航路>终端>NPA: %v", hals)
	}
}

func TestValidate(t *testing.T) {
	base := profile.Profile{
		Name: "x", Pfa: 1e-5, Pmd: 1e-3, HAL: 100,
		Isolation: profile.Isolation{MinEpochs: 2},
		Alert:     profile.Alert{Mode: profile.Persistence, ConfirmEpochs: 2, ClearEpochs: 2},
	}
	cases := []struct {
		mutate func(*profile.Profile)
		field  string
	}{
		{func(p *profile.Profile) { p.Name = "" }, "name"},
		{func(p *profile.Profile) { p.Pfa = 0 }, "pfa"},
		{func(p *profile.Profile) { p.Pmd = 1 }, "pmd"},
		{func(p *profile.Profile) { p.HAL = -1 }, "hal"},
		{func(p *profile.Profile) { p.Isolation.MinEpochs = 0 }, "isolation.min_epochs"},
		{func(p *profile.Profile) { p.Alert.ConfirmEpochs = 0 }, "alert.confirm_epochs"},
		{func(p *profile.Profile) { p.Alert.ClearEpochs = 0 }, "alert.clear_epochs"},
		{func(p *profile.Profile) { p.Alert.Mode = "weird" }, "alert.mode"},
		{func(p *profile.Profile) {
			p.Alert.Mode = profile.Combined
			p.Alert.GrossFactor = 1
		}, "alert.gross_factor"},
	}
	for i, c := range cases {
		p := base
		c.mutate(&p)
		err := p.Validate()
		fe, ok := err.(*apierr.FieldError)
		if !ok {
			t.Fatalf("case %d: 期望 FieldError, got %v", i, err)
		}
		if fe.Field != c.field {
			t.Fatalf("case %d: 字段=%s 期望 %s", i, fe.Field, c.field)
		}
	}
}
