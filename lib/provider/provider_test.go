package provider

import (
	"testing"
	"time"
)

func TestCompareVersion(t *testing.T) {
	t.Parallel()
	cases := []struct {
		a, b string
		want int
	}{
		{"v4.9.0", "v4.10.0", -1},
		{"v4.10.0", "v4.9.0", 1},
		{"v5.0.1", "v4.81.0", 1},
		{"v4.62.1", "v4.62.0", 1},
		{"v2.0.0", "v2.0.0", 0},
		{"v1.2.3-rc1", "v1.2.3", -1},
	}
	for _, c := range cases {
		if got := CompareVersion(c.a, c.b); (got < 0) != (c.want < 0) || (got > 0) != (c.want > 0) {
			t.Errorf("CompareVersion(%q, %q) = %d, want sign of %d", c.a, c.b, got, c.want)
		}
	}
}

func TestSample(t *testing.T) {
	t.Parallel()
	rs := make([]Release, 0, 10)
	for i := range 10 {
		rs = append(rs, Release{Version: "v1." + string(rune('0'+i)) + ".0", Date: time.Now()})
	}
	got := Sample(rs, 4)
	want := []string{"v1.0.0", "v1.4.0", "v1.8.0", "v1.9.0"}
	if len(got) != len(want) {
		t.Fatalf("Sample(10, 4) returned %d releases, want %d", len(got), len(want))
	}
	for i, w := range want {
		if got[i].Version != w {
			t.Errorf("Sample[%d] = %s, want %s", i, got[i].Version, w)
		}
	}
	if len(Sample(rs, 1)) != 10 || len(Sample(rs, 0)) != 10 {
		t.Error("Sample with n <= 1 should return every release")
	}
}

func TestDropBackports(t *testing.T) {
	t.Parallel()
	d := func(s string) time.Time { tm, _ := time.Parse("2006-01-02", s); return tm }
	rs := []Release{
		{Version: "v4.14.0", Date: d("2025-01-09")},
		{Version: "v3.117.1", Date: d("2025-02-28")}, // backport on the old line
		{Version: "v4.15.0", Date: d("2025-01-16")},
		{Version: "v5.0.0", Date: d("2026-07-28")},
	}
	got := DropBackports(rs)
	if len(got) != 3 || got[0].Version != "v4.14.0" || got[1].Version != "v4.15.0" || got[2].Version != "v5.0.0" {
		t.Errorf("DropBackports = %v", got)
	}
}

func TestPaths(t *testing.T) {
	t.Parallel()
	p, err := New("azurerm", "", ".cache", "data")
	if err != nil {
		t.Fatal(err)
	}
	if p.Repo != "hashicorp/terraform-provider-azurerm" || p.Source != "hashicorp/azurerm" {
		t.Errorf("unexpected defaults: %+v", p)
	}
	if got := p.ZipURL("v5.4.0", "linux_amd64"); got != "https://releases.hashicorp.com/terraform-provider-azurerm/5.4.0/terraform-provider-azurerm_5.4.0_linux_amd64.zip" {
		t.Errorf("ZipURL = %s", got)
	}
}
