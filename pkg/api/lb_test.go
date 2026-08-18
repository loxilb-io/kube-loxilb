package api

import "testing"

// The numeric value of each EpSelect IS the wire contract: it is sent verbatim
// as serviceArguments.sel. These are the values loxilb assigns in
// common/common.go, so pin them against an accidental reordering -- inserting a
// constant mid-block silently repoints every annotation after it at a different
// algorithm, with no compile error and no server error.
func TestEpSelectWireValues(t *testing.T) {
	tests := []struct {
		name string
		got  EpSelect
		want EpSelect
	}{
		{"LbSelRr", LbSelRr, 0},
		{"LbSelHash", LbSelHash, 1},
		{"LbSelPrio", LbSelPrio, 2},
		{"LbSelRrPersist", LbSelRrPersist, 3},
		{"LbSelLeastConnections", LbSelLeastConnections, 4},
		{"LbSelN2", LbSelN2, 5},
		{"LbSelN3", LbSelN3, 6},
		// 7 is reserved by loxilb and intentionally unnamed
		{"LbSelCHWBL", LbSelCHWBL, 8},
		{"LbSelGPUAware", LbSelGPUAware, 9},
		{"LbSelWRRHash", LbSelWRRHash, 10},
	}

	for _, tt := range tests {
		if tt.got != tt.want {
			t.Errorf("%s = %d, want %d", tt.name, tt.got, tt.want)
		}
	}
}

func TestLbModeWireValues(t *testing.T) {
	if LBModeFullProxy != 4 {
		t.Errorf("LBModeFullProxy = %d, want 4", LBModeFullProxy)
	}
}
