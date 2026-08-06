package main

import "testing"

func TestParseRam(t *testing.T) {
	cases := []struct {
		in   string
		want uint64
	}{
		{"32GB", 32 * GiB},
		{"16384MB", 16 * GiB},
		{"512KB", 512 * KiB},
		{"16777216", 16 * MiB}, // plain bytes
		{" 8GB ", 8 * GiB},     // whitespace tolerated
	}
	for _, c := range cases {
		if got, err := parseRam(c.in); err != nil || got != c.want {
			t.Errorf("parseRam(%q) = %d, %v; want %d", c.in, got, err, c.want)
		}
	}
	for _, in := range []string{"", "1.5GB", "32G", "lots"} {
		if _, err := parseRam(in); err == nil {
			t.Errorf("parseRam(%q) should error", in)
		}
	}
}

func TestMemTotalFrom(t *testing.T) {
	content := "MemTotal:       16777216 kB\nMemFree:         1000000 kB\n"
	got, err := memTotalFrom(content)
	if err != nil || got != 16777216 {
		t.Errorf("memTotalFrom = %d, %v; want 16777216", got, err)
	}
	if _, err := memTotalFrom("no memtotal here\n"); err == nil {
		t.Error("memTotalFrom should error on missing MemTotal")
	}
}

func TestCPUCountFrom(t *testing.T) {
	content := "processor\t: 0\nmodel name : X\nprocessor\t: 1\nmodel name : X\n"
	if got := cpuCountFrom(content); got != 2 {
		t.Errorf("cpuCountFrom = %d, want 2", got)
	}
	if got := cpuCountFrom("no processors\n"); got != 0 {
		t.Errorf("cpuCountFrom(empty) = %d, want 0", got)
	}
}

func TestLocalHost(t *testing.T) {
	for _, local := range []string{"", "localhost", "127.0.0.1", "::1", "/var/run/postgresql", "/tmp"} {
		if !localHost(local) {
			t.Errorf("localHost(%q) = false, want true", local)
		}
	}
	for _, remote := range []string{"db.internal.example", "10.0.0.5", "pg.example.com"} {
		if localHost(remote) {
			t.Errorf("localHost(%q) = true, want false", remote)
		}
	}
}

func TestDeviceForPathFromMounts(t *testing.T) {
	mounts := "/dev/mapper/vg-root / ext4 rw 0 0\n" +
		"/dev/nvme0n1p1 /boot ext4 rw 0 0\n" +
		"/dev/sda1 /var/lib/postgresql ext4 rw 0 0\n" +
		"overlay /app overlay rw 0 0\n"
	// longest matching mount wins, overlay is skipped
	if got := deviceForPathFromMounts("/var/lib/postgresql/17/main/base", mounts); got != "sda" {
		t.Errorf("deviceForPathFromMounts(data) = %q, want sda", got)
	}
	if got := deviceForPathFromMounts("/boot/grub", mounts); got != "nvme0n1" {
		t.Errorf("deviceForPathFromMounts(/boot) = %q, want nvme0n1", got)
	}
	// component boundary: /var/libx must not match the /var/lib/postgresql
	// mount; it falls through to the root device
	if got := deviceForPathFromMounts("/var/libx/foo", mounts); got != "mapper/vg-root" {
		t.Errorf("deviceForPathFromMounts(/var/libx) = %q, want root device", got)
	}
	// any path is at least under the root mount
	if got := deviceForPathFromMounts("/unmounted", mounts); got != "mapper/vg-root" {
		t.Errorf("deviceForPathFromMounts(/unmounted) = %q, want root device", got)
	}
}

func TestDeviceName(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/dev/sda1", "sda"},
		{"/dev/vda", "vda"},
		{"/dev/xvda1", "xvda"},
		{"/dev/nvme0n1p1", "nvme0n1"},
		{"/dev/nvme0n1", "nvme0n1"},
		{"/dev/mmcblk0p1", "mmcblk0"},
		{"/dev/mapper/vg-root", "mapper/vg-root"}, // unknown shape passes through
	}
	for _, c := range cases {
		if got := deviceName(c.in); got != c.want {
			t.Errorf("deviceName(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestRotationalFromContent(t *testing.T) {
	if r, ok := rotationalFromContent("0\n"); !ok || r != 0 {
		t.Errorf("rotationalFromContent(0) = %d, %v; want 0, true", r, ok)
	}
	if r, ok := rotationalFromContent("1"); !ok || r != 1 {
		t.Errorf("rotationalFromContent(1) = %d, %v; want 1, true", r, ok)
	}
	if _, ok := rotationalFromContent("junk\n"); ok {
		t.Error("rotationalFromContent(junk) should be !ok")
	}
}
