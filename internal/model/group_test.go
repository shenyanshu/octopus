package model

import "testing"

// IsValidGroupMode 是备份导入等无法复用 binding 标签的校验点的单点真相。
func TestIsValidGroupMode(t *testing.T) {
	cases := []struct {
		mode GroupMode
		want bool
	}{
		{GroupModeManual, true},
		{GroupModeFailover, true},
		{GroupModeScored, true},
		{GroupMode(""), false},
		{GroupMode("unknown"), false},
		{GroupMode("MANUAL"), false},
	}
	for _, tc := range cases {
		if got := IsValidGroupMode(tc.mode); got != tc.want {
			t.Errorf("IsValidGroupMode(%q) = %v, 想要 %v", tc.mode, got, tc.want)
		}
	}
}
