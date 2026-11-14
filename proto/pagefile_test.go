package proto

import "testing"

func TestPageFile(t *testing.T) {
	cases := []struct {
		p       PageFile
		ok      bool
		value   string
		write   bool
		reserve uint64 // with 16 GiB of memory
	}{
		{PageFile{Mode: PageFileImage}, true, "", false, 0},
		{PageFile{Mode: PageFileAuto}, true, `?:\pagefile.sys`, true, 0},
		{PageFile{Mode: PageFileOff}, true, "", true, 0},
		{PageFile{Mode: PageFileFixed, MinMB: 4096, MaxMB: 8192}, true, `C:\pagefile.sys 4096 8192`, true, 0},
		{PageFile{Mode: PageFileFixed, MinMB: 16384, MaxMB: 32768}, true, `C:\pagefile.sys 16384 32768`, true, 16 << 30},
		{PageFile{Mode: PageFileFixed, MinMB: 8192, MaxMB: 4096}, false, "", false, 0},
		{PageFile{Mode: PageFileFixed, MinMB: 1, MaxMB: 4096}, false, "", false, 0},
		{PageFile{Mode: "swap"}, false, "", false, 0},
	}
	for _, c := range cases {
		if err := c.p.Check(); (err == nil) != c.ok {
			t.Errorf("%+v: check %v", c.p, err)
		}
		if !c.ok {
			continue
		}
		value, write := c.p.PagingFiles()
		if value != c.value || write != c.write {
			t.Errorf("%+v: %q %v", c.p, value, write)
		}
		if got := c.p.Reserve(16 << 30); got != c.reserve {
			t.Errorf("%+v: reserve %d", c.p, got)
		}
	}
}
