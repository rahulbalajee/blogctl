package cli

import (
	"runtime/debug"
	"testing"
)

func TestResolveVersion(t *testing.T) {
	installed := &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}
	local := &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}
	for _, tc := range []struct {
		stamped string
		info    *debug.BuildInfo
		want    string
	}{
		{"v0.2.0-3-gabc1234", installed, "v0.2.0-3-gabc1234"},
		{"dev", installed, "v0.1.0"},
		{"", installed, "v0.1.0"},
		{"dev", local, "dev"},
		{"dev", nil, "dev"},
	} {
		if got := resolveVersion(tc.stamped, tc.info); got != tc.want {
			t.Errorf("resolveVersion(%q, %v) = %q, want %q", tc.stamped, tc.info, got, tc.want)
		}
	}
}
