package discordbot

import "testing"

func TestHumanSize(t *testing.T) {
	cases := map[int]string{
		0:               "0 B",
		850:             "850 B",
		46 * 1024:       "46 KB",
		1024*1024 - 100: "1024 KB",
		3 * 1024 * 1024: "3.0 MB",
	}
	for n, want := range cases {
		if got := humanSize(n); got != want {
			t.Errorf("humanSize(%d) = %q, se esperaba %q", n, got, want)
		}
	}
}
