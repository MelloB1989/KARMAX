package console

import "testing"

func TestHostParsesProcFixtures(t *testing.T) {
	h, err := readHost("testdata/proc")
	if err != nil {
		t.Fatal(err)
	}
	if h.Load != [3]float64{0, 0.05, 0} {
		t.Fatalf("load = %v", h.Load)
	}
	if h.MemTotal != 16384000*1024 || h.MemUsed != (16384000-10384000)*1024 {
		t.Fatalf("mem = %d used of %d", h.MemUsed, h.MemTotal)
	}
	if h.UptimeS != 86400 || h.CPUs < 1 {
		t.Fatalf("uptime %d cpus %d", h.UptimeS, h.CPUs)
	}
}

func TestHostFailsOnMissingProc(t *testing.T) {
	if _, err := readHost("testdata/nope"); err == nil {
		t.Fatal("want an error")
	}
}
