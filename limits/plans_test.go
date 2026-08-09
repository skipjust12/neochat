package limits

import "testing"

func TestLoadPlanLimits(t *testing.T) {
	plans, err := LoadPlanLimits("../configs/plans.json")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	pro, ok := plans["pro"]
	if !ok {
		t.Fatal("expected a \"pro\" plan in configs/plans.json")
	}
	if pro.ThinkingMaxCapUSD != 10.0 || pro.InstantExtraCapUSD != 3.0 {
		t.Errorf("pro plan = %+v, want thinking_max_cap_usd=10, instant_extra_cap_usd=3", pro)
	}

	for _, id := range []string{"pro", "pro_plus", "max"} {
		if _, ok := plans[id]; !ok {
			t.Errorf("expected plan %q in configs/plans.json", id)
		}
	}
}

func TestLoadPlanLimits_MissingFile(t *testing.T) {
	if _, err := LoadPlanLimits("../configs/does-not-exist.json"); err == nil {
		t.Fatal("expected an error for a missing plans file")
	}
}
