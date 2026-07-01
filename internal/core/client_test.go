package core

import "testing"

func TestExpandPerpDexs(t *testing.T) {
	avail := []string{"", "xyz", "flx", "vntl"} // index 0 = core dex ("")
	tests := []struct {
		name       string
		configured []string
		want       []string
	}{
		{"wildcard all", []string{"all"}, []string{"xyz", "flx", "vntl"}},
		{"wildcard star", []string{"*"}, []string{"xyz", "flx", "vntl"}},
		{"wildcard case-insensitive", []string{"ALL"}, []string{"xyz", "flx", "vntl"}},
		{"wildcard wins over names", []string{"xyz", "all"}, []string{"xyz", "flx", "vntl"}},
		{"explicit unchanged", []string{"xyz"}, []string{"xyz"}},
		{"multiple explicit unchanged", []string{"xyz", "flx"}, []string{"xyz", "flx"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := expandPerpDexs(tc.configured, avail)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}
