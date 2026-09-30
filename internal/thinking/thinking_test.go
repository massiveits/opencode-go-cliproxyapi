package thinking

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"

	"opencode-go-cliproxyapi/internal/errclass"
)

func TestCanonicalLevelsOrder(t *testing.T) {
	want := []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}
	if len(CanonicalLevels) != len(want) {
		t.Fatalf("CanonicalLevels = %v", CanonicalLevels)
	}
	for i := range want {
		if CanonicalLevels[i] != want[i] {
			t.Fatalf("CanonicalLevels = %v, want %v", CanonicalLevels, want)
		}
	}
}

func TestSupportedLevelsFilterAndOrder(t *testing.T) {
	cases := []struct {
		name string
		ts   *pluginapi.ThinkingSupport
		want []string
	}{
		{"nil ts", nil, []string{"low", "medium", "high"}},
		{"empty levels", &pluginapi.ThinkingSupport{}, []string{"low", "medium", "high"}},
		{"all non-canonical", &pluginapi.ThinkingSupport{Levels: []string{"turbo", "mega"}}, []string{"low", "medium", "high"}},
		{
			"reordered to canonical",
			&pluginapi.ThinkingSupport{Levels: []string{"xhigh", "bogus", " none ", "NONE", "Minimal"}},
			[]string{"none", "minimal", "xhigh"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := SupportedLevels(c.ts)
			if len(got) != len(c.want) {
				t.Fatalf("SupportedLevels = %v, want %v", got, c.want)
			}
			for i := range c.want {
				if got[i] != c.want[i] {
					t.Fatalf("SupportedLevels = %v, want %v", got, c.want)
				}
			}
		})
	}
}

func TestEffortFromBudgetDisabledBudget(t *testing.T) {
	cases := []struct {
		name string
		b    int64
		ts   *pluginapi.ThinkingSupport
		want string
	}{
		{"nil ts zero budget", 0, nil, "low"},
		{"nil ts negative budget", -1, nil, "low"},
		{"zero allowed with none", 0,
			&pluginapi.ThinkingSupport{ZeroAllowed: true, Levels: []string{"none", "low", "high"}}, "none"},
		{"zero allowed without none falls to dynamic", 0,
			&pluginapi.ThinkingSupport{ZeroAllowed: true, DynamicAllowed: true, Levels: []string{"low", "high"}}, "auto"},
		{"zero allowed without none and no dynamic takes lowest", 0,
			&pluginapi.ThinkingSupport{ZeroAllowed: true, Levels: []string{"low", "medium"}}, "low"},
		{"dynamic allowed returns auto regardless of list", -1,
			&pluginapi.ThinkingSupport{DynamicAllowed: true}, "auto"},
		{"neither flag takes lowest", 0,
			&pluginapi.ThinkingSupport{Levels: []string{"medium", "high"}}, "medium"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffortFromBudget(c.b, c.ts); got != c.want {
				t.Errorf("EffortFromBudget(%d, %+v) = %q, want %q", c.b, c.ts, got, c.want)
			}
		})
	}
}

// TestEffortFromBudgetThresholdTable pins the ported host threshold mapping
// (SDK convert.go @ v8.0.0: <=512 minimal, <=1024 low, <=8192 medium,
// <=24576 high, else xhigh) followed by nearest-supported clamping.
func TestEffortFromBudgetThresholdTable(t *testing.T) {
	cases := []struct {
		name string
		b    int64
		ts   *pluginapi.ThinkingSupport
		want string
	}{
		{"upper minimal bound stays minimal", 512,
			&pluginapi.ThinkingSupport{Levels: []string{"minimal", "low", "medium"}}, "minimal"},
		{"just past minimal is low", 513,
			&pluginapi.ThinkingSupport{Levels: []string{"minimal", "low", "medium"}}, "low"},
		{"upper low bound", 1024,
			&pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high"}}, "low"},
		{"just past low is medium", 1025,
			&pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high"}}, "medium"},
		{"upper medium bound", 8192,
			&pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high"}}, "medium"},
		{"just past medium is high", 8193,
			&pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high"}}, "high"},
		{"upper high bound", 24576,
			&pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high"}}, "high"},
		{"past high is xhigh", 24577,
			&pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh"}}, "xhigh"},
		{"huge budget derives xhigh", 999999,
			&pluginapi.ThinkingSupport{Levels: CanonicalLevels}, "xhigh"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffortFromBudget(c.b, c.ts); got != c.want {
				t.Errorf("EffortFromBudget(%d, %+v) = %q, want %q", c.b, c.ts, got, c.want)
			}
		})
	}
}

// TestEffortFromBudgetClampToSupported pins the nearest-index clamp rule:
// distance in CanonicalLevels order, weaker side wins ties. The auditor case
// (budget 8192 → derived "medium" → equidistant low/high → weaker "low") is
// the first entry.
func TestEffortFromBudgetClampToSupported(t *testing.T) {
	cases := []struct {
		name string
		b    int64
		ts   *pluginapi.ThinkingSupport
		want string
	}{
		{"auditor case medium clamps to weaker low on tie", 8192,
			&pluginapi.ThinkingSupport{Max: 24576, Levels: []string{"low", "high"}}, "low"},
		{"medium nearer high clamps up", 8192,
			&pluginapi.ThinkingSupport{Levels: []string{"high"}}, "high"},
		{"xhigh clamps to high not max", 999999,
			&pluginapi.ThinkingSupport{Levels: []string{"low", "high", "max"}}, "high"},
		{"minimal clamps to none when only none and high", 1,
			&pluginapi.ThinkingSupport{Levels: []string{"none", "high"}}, "none"},
		{"single level always wins", 12345,
			&pluginapi.ThinkingSupport{Max: 20000, Levels: []string{"high"}}, "high"},
		{"derived level present passes through", 20000,
			&pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high"}}, "high"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := EffortFromBudget(c.b, c.ts); got != c.want {
				t.Errorf("EffortFromBudget(%d, %+v) = %q, want %q", c.b, c.ts, got, c.want)
			}
		})
	}
}

// TestEffortFromBudgetMaxIgnored pins that the mapping is threshold-driven,
// not proportional to the declared Max (the host tables carry no Max term).
func TestEffortFromBudgetMaxIgnored(t *testing.T) {
	small := &pluginapi.ThinkingSupport{Max: 4096, Levels: []string{"low", "medium", "high"}}
	large := &pluginapi.ThinkingSupport{Max: 128000, Levels: []string{"low", "medium", "high"}}
	for _, b := range []int64{1, 4095, 4096, 8192, 8193, 24576, 99999} {
		if got, want := EffortFromBudget(b, small), EffortFromBudget(b, large); got != want {
			t.Errorf("EffortFromBudget(%d) differs by Max: %q vs %q", b, got, want)
		}
	}
}

func TestBudgetFromEffort(t *testing.T) {
	cases := []struct {
		name       string
		effort     string
		ts         *pluginapi.ThinkingSupport
		wantBudget int64
		wantOK     bool
	}{
		{"auto dynamic sentinel", "auto", &pluginapi.ThinkingSupport{DynamicAllowed: true}, -1, true},
		{"auto normalized", "  AUTO\t", &pluginapi.ThinkingSupport{DynamicAllowed: true}, -1, true},
		{"auto without dynamic falls to medium table value", "auto",
			&pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high"}}, 8192, true},
		{"auto with nil capability", "auto", nil, 8192, true},
		{"auto middle of pair clamps to weaker low", "auto",
			&pluginapi.ThinkingSupport{Levels: []string{"low", "high"}}, 1024, true},
		{"none with zero allowed", " None ", &pluginapi.ThinkingSupport{ZeroAllowed: true}, 0, true},
		{"none with zero allowed ignores Min", "none",
			&pluginapi.ThinkingSupport{ZeroAllowed: true, Min: 2048}, 0, true},
		{"minimal with zero allowed keeps table value", "minimal",
			&pluginapi.ThinkingSupport{ZeroAllowed: true}, 512, true},
		{"none without zero takes weakest enabled", "none",
			&pluginapi.ThinkingSupport{Levels: []string{"low", "high"}}, 1024, true},
		{"minimal without zero takes weakest enabled", "minimal",
			&pluginapi.ThinkingSupport{ZeroAllowed: false, Levels: []string{"low", "high"}}, 1024, true},
		{"unknown effort rejected", "bogus", nil, 0, false},
		{"effort outside supported rejected", "high", &pluginapi.ThinkingSupport{Levels: []string{"low"}}, 0, false},
		{"empty effort rejected", "", nil, 0, false},
		{"exact table value low", "low", nil, 1024, true},
		{"exact table value high", "high", nil, 24576, true},
		{"exact table value xhigh", " XHigh ",
			&pluginapi.ThinkingSupport{Levels: []string{"low", "medium", "high", "xhigh", "max"}}, 32768, true},
		{"exact table value max", "max",
			&pluginapi.ThinkingSupport{Levels: []string{"low", "max"}}, 128000, true},
		{"table value clamped down to max", "max",
			&pluginapi.ThinkingSupport{Min: 1000, Max: 2000, Levels: []string{"low", "max"}}, 2000, true},
		{"table value clamped up to min", "low",
			&pluginapi.ThinkingSupport{Min: 4096, Max: 32768, Levels: []string{"low", "high"}}, 4096, true},
		{"degenerate bounds clamp to min", "high",
			&pluginapi.ThinkingSupport{Min: 5000, Max: 5000, Levels: []string{"low", "medium", "high"}}, 5000, true},
		{"non-positive bounds ignored", "medium",
			&pluginapi.ThinkingSupport{Min: -5, Max: 0, Levels: []string{"low", "medium", "high"}}, 8192, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			budget, ok := BudgetFromEffort(c.effort, c.ts)
			if ok != c.wantOK || budget != c.wantBudget {
				t.Errorf("BudgetFromEffort(%q, %+v) = (%d,%t), want (%d,%t)",
					c.effort, c.ts, budget, ok, c.wantBudget, c.wantOK)
			}
		})
	}
}

// TestEffortNeverOutsideSupported sweeps budgets against varied capabilities
// asserting every result is a supported level or the "auto" sentinel (FR-005).
func TestEffortNeverOutsideSupported(t *testing.T) {
	capabilities := []*pluginapi.ThinkingSupport{
		nil,
		{},
		{Min: 128, Max: 20000, ZeroAllowed: false, DynamicAllowed: true},
		{Min: 1024, Max: 128000, ZeroAllowed: true, DynamicAllowed: false, Levels: []string{"low", "medium", "high", "max"}},
		{Levels: []string{"none", "minimal", "low", "medium", "high", "xhigh", "max"}},
		{Levels: []string{"HIGH"}},
		{Levels: []string{"warp", "turbo"}},
		{Max: 0, ZeroAllowed: true, DynamicAllowed: true},
	}
	isSupported := func(levels []string, level string) bool {
		for _, l := range levels {
			if l == level {
				return true
			}
		}
		return false
	}
	for _, ts := range capabilities {
		supported := SupportedLevels(ts)
		for b := int64(-100); b <= 150000; b += 997 {
			got := EffortFromBudget(b, ts)
			if got == "auto" {
				continue
			}
			if !isSupported(supported, got) {
				t.Fatalf("budget %d with %+v produced %q, outside supported %v", b, ts, got, supported)
			}
		}
	}
}

// TestBudgetFromEffortRoundTrip pins table-level consistency: converting a
// level to its budget and back yields the same level whenever it is
// supported. ZeroAllowed keeps "none"/"minimal" on their own table values;
// "max" is excluded because the host never derives it from a budget (its
// 128000 table value maps back through the thresholds to "xhigh").
func TestBudgetFromEffortRoundTrip(t *testing.T) {
	ts := &pluginapi.ThinkingSupport{ZeroAllowed: true, Levels: CanonicalLevels}
	for _, level := range []string{"none", "minimal", "low", "medium", "high", "xhigh"} {
		budget, ok := BudgetFromEffort(level, ts)
		if !ok {
			t.Fatalf("BudgetFromEffort(%q) not ok", level)
		}
		if got := EffortFromBudget(budget, ts); got != level {
			t.Errorf("round trip %q → %d → %q", level, budget, got)
		}
	}
}

// F10 pin: ValidateEffort is the one shared reject kernel — the exact
// semantics (and message shape) previously duplicated in both adapters'
// request normalizers, wrapped here through a single table.
func TestValidateEffort(t *testing.T) {
	cases := []struct {
		name    string
		effort  string
		ts      *pluginapi.ThinkingSupport
		wantMsg string // "" → nil error expected
	}{
		{"nil ts default ladder admits high", "high", nil, ""},
		{"case and space normalized", "  HIGH ", nil, ""},
		{"default ladder rejects xhigh naming the raw value", " XHigh ", nil,
			`reasoning_effort " XHigh " is not supported for this model`},
		{"declared level admitted", "xhigh", &pluginapi.ThinkingSupport{Levels: []string{"low", "high", "xhigh"}}, ""},
		{"auto rejected unless declared", "auto", nil,
			`reasoning_effort "auto" is not supported for this model`},
		{"auto never admissible: non-canonical levels are filtered", "auto",
			&pluginapi.ThinkingSupport{Levels: []string{"low", "auto"}},
			`reasoning_effort "auto" is not supported for this model`},
		{"none rejected unless declared", "none", nil,
			`reasoning_effort "none" is not supported for this model`},
		{"none admitted when declared", "none", &pluginapi.ThinkingSupport{Levels: []string{"none", "low"}}, ""},
		{"none admitted under ZeroAllowed without declaration", "none",
			&pluginapi.ThinkingSupport{ZeroAllowed: true}, ""},
		{"auto admitted under DynamicAllowed", "auto", &pluginapi.ThinkingSupport{DynamicAllowed: true}, ""},
		{"empty is unsupported", "", nil,
			`reasoning_effort "" is not supported for this model`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			eErr := ValidateEffort(tc.effort, tc.ts)
			if tc.wantMsg == "" {
				if eErr != nil {
					t.Fatalf("want nil, got %+v", eErr)
				}
				return
			}
			if eErr == nil || eErr.Class != errclass.ClassUnsupported || eErr.Message != tc.wantMsg {
				t.Fatalf("err = %+v, want ClassUnsupported %q", eErr, tc.wantMsg)
			}
		})
	}
}

// TestBudgetToLevelTable pins the ported threshold constants themselves.
func TestBudgetToLevelTable(t *testing.T) {
	cases := []struct {
		b    int64
		want string
	}{
		{-1, "minimal"}, // sentinel handled by caller; table starts at minimal
		{0, "minimal"},
		{512, "minimal"},
		{513, "low"},
		{1024, "low"},
		{1025, "medium"},
		{8192, "medium"},
		{8193, "high"},
		{24576, "high"},
		{24577, "xhigh"},
	}
	for _, c := range cases {
		if got := budgetToLevel(c.b); got != c.want {
			t.Errorf("budgetToLevel(%d) = %q, want %q", c.b, got, c.want)
		}
	}
}

// clampToSupported fallbacks: nearest canonical wins by distance (ties ->
// lower index). Inputs are always canonical per the domain contract.
func TestClampToSupportedNearestMatch(t *testing.T) {
	supported := []string{"low", "xhigh"}
	if got := clampToSupported("medium", supported); got != "low" {
		t.Fatalf("nearest to medium = %q, want low", got)
	}
	if got := clampToSupported("max", supported); got != "xhigh" {
		t.Fatalf("nearest to max = %q, want xhigh", got)
	}
	if got := clampToSupported("medium", []string{"low", "high"}); got != "low" {
		t.Fatalf("tie = %q, want weaker low", got)
	}
}
