package sysinfo

import "testing"

func TestFixtures(t *testing.T) {
	Root = "testdata"
	t.Cleanup(func() { Root = "/" })

	m, err := Meminfo()
	if err != nil {
		t.Fatal(err)
	}
	if want := (Mem{Total: 16384000 * 1024, Available: 4096000 * 1024, SwapTotal: 8192000 * 1024, SwapFree: 8000000 * 1024}); m != want {
		t.Errorf("Meminfo = %+v, want %+v", m, want)
	}
	if m.Used() != (16384000-4096000)*1024 {
		t.Errorf("Used = %d", m.Used())
	}

	p, err := Pressure("memory")
	if err != nil {
		t.Fatal(err)
	}
	if p != 12.5 {
		t.Errorf("Pressure = %v, want 12.5", p)
	}

	if n, err := OOMKills(); err != nil || n != 3 {
		t.Errorf("OOMKills = %d, %v; want 3", n, err)
	}

	var c CPU
	busy, err := c.Busy()
	if err != nil {
		t.Fatal(err)
	}
	// 200 busy (user+system) of 1000; iowait counts as idle.
	if busy != 0.2 {
		t.Errorf("Busy = %v, want 0.2", busy)
	}
	if again, _ := c.Busy(); again != 0 {
		t.Errorf("Busy with no time passed = %v, want 0", again)
	}
}

// Commands in a slice under a capped user.slice get its memory, when it is
// fuller than the host's.
func TestMemoryOfTheTightestDomain(t *testing.T) {
	Root = "testdata"
	t.Cleanup(func() { Root = "/" })
	m, err := Memory("/user.slice/user-hotbed.slice")
	if err != nil {
		t.Fatal(err)
	}
	want := Mem{Total: 8 << 30, Available: 1 << 30, SwapTotal: 8192000 * 1024, SwapFree: 8000000 * 1024, Domain: "/user.slice"}
	if m != want {
		t.Errorf("Memory = %+v, want %+v", m, want)
	}
	host, err := Memory("/hotbed.slice")
	if err != nil {
		t.Fatal(err)
	}
	if host.Domain != "" || host.Total != 16384000*1024 {
		t.Errorf("Memory outside any limit = %+v, want the host's", host)
	}
}
