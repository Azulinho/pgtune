package main

import (
	"fmt"
	"testing"
)

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

func TestParseDiskOverride(t *testing.T) {
	cases := []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "auto", true},
		{"auto", "auto", true},
		{"AUTO", "auto", true},
		{"ssd", "ssd", true},
		{" SSD ", "ssd", true},
		{"hdd", "hdd", true},
		{"nvme", "", false},
	}
	for _, c := range cases {
		got, err := parseDiskOverride(c.in)
		if (err == nil) != c.ok || (c.ok && got != c.want) {
			t.Errorf("parseDiskOverride(%q) = %q, %v; want %q, ok=%v", c.in, got, err, c.want, c.ok)
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

// fakeSysFS serves a synthetic /sys tree so detection logic is testable
// without touching the real host.
type fakeSysFS struct {
	files map[string]string
	dirs  map[string][]string
}

func (f fakeSysFS) ReadFile(path string) (string, error) {
	v, ok := f.files[path]
	if !ok {
		return "", fmt.Errorf("fake sysfs: no file %q", path)
	}
	return v, nil
}

func (f fakeSysFS) ListDir(path string) ([]string, error) {
	v, ok := f.dirs[path]
	if !ok {
		return nil, fmt.Errorf("fake sysfs: no dir %q", path)
	}
	return v, nil
}

func TestMountForPath(t *testing.T) {
	mounts := parseMounts("/dev/sda1 / ext4 rw 0 0\n" +
		"/dev/nvme0n1p1 /boot ext4 rw 0 0\n" +
		"overlay /var/lib/docker/overlay2/abc/merged overlay rw,upperdir=/var/lib/docker/overlay2/abc/diff,workdir=/x 0 0\n")

	// longest matching mount wins: the overlay mount, not the root device
	if m, ok := mountForPath("/var/lib/docker/overlay2/abc/merged/foo", mounts); !ok || m.fstype != "overlay" {
		t.Errorf("mountForPath(overlay) = %+v, %v; want overlay mount", m, ok)
	}
	if m, ok := mountForPath("/boot/grub", mounts); !ok || m.source != "/dev/nvme0n1p1" {
		t.Errorf("mountForPath(/boot) = %+v, %v; want nvme source", m, ok)
	}
	// component boundary: /var/libx must not match the overlay mount
	if m, ok := mountForPath("/var/libx/foo", mounts); !ok || m.source != "/dev/sda1" {
		t.Errorf("mountForPath(/var/libx) = %+v, %v; want root device", m, ok)
	}
	if _, ok := mountForPath("relative/path", mounts); ok {
		t.Error("mountForPath(relative) should not match")
	}
}

func TestParseMountsUnescape(t *testing.T) {
	mounts := parseMounts("/dev/sda1 /mnt/with\\040space ext4 rw 0 0\n")
	if len(mounts) != 1 || mounts[0].point != "/mnt/with space" {
		t.Errorf("parseMounts unescape = %+v, want /mnt/with space", mounts)
	}
}

func TestDeviceNameFromSource(t *testing.T) {
	fs := fakeSysFS{
		files: map[string]string{
			"block/dm-0/dm/name": "vg-root\n",
			"block/dm-1/dm/name": "other-lv\n",
		},
		dirs: map[string][]string{
			"block": {"dm-0", "dm-1", "sda"},
		},
	}
	cases := []struct {
		source string
		want   string
		ok     bool
	}{
		{"/dev/sda1", "sda", true},
		{"/dev/nvme0n1p1", "nvme0n1", true},
		{"/dev/mmcblk0p1", "mmcblk0", true},
		{"/dev/dm-0", "dm-0", true},
		{"/dev/md0", "md0", true},
		{"/dev/mapper/vg-root", "dm-0", true},
		{"/dev/vg/root", "dm-0", true},
		{"/dev/disk/by-uuid/1234", "", false},
		{"overlay", "", false},
	}
	for _, c := range cases {
		got, ok := deviceNameFromSource(fs, c.source)
		if got != c.want || ok != c.ok {
			t.Errorf("deviceNameFromSource(%q) = %q, %v; want %q, %v", c.source, got, ok, c.want, c.ok)
		}
	}
}

func TestStorageClassFromRotational(t *testing.T) {
	cases := []struct {
		in   []int
		want string
	}{
		{[]int{0, 0}, "ssd"},
		{[]int{1, 1}, "hdd"},
		{[]int{0, 1}, "unknown"}, // mixed storage: do not assume SSD
		{[]int{0, -1}, "unknown"},
		{nil, "unknown"},
	}
	for _, c := range cases {
		if got := storageClassFromRotational(c.in); got != c.want {
			t.Errorf("storageClassFromRotational(%v) = %q, want %q", c.in, got, c.want)
		}
	}
}

func TestDetectDiskTypeWith(t *testing.T) {
	fs := fakeSysFS{
		files: map[string]string{
			"block/dm-0/dm/name":             "vg-root\n",
			"block/dm-1/dm/name":             "mixed-lv\n",
			"block/sda/queue/rotational":     "1\n",
			"block/nvme0n1/queue/rotational": "0\n",
		},
		dirs: map[string][]string{
			"block":              {"dm-0", "dm-1", "sda", "nvme0n1", "loop0"},
			"block/dm-0/slaves":  {"sda"},
			"block/dm-1/slaves":  {"sda", "nvme0n1"},
			"block/loop0/slaves": {},
		},
	}
	mounts := "/dev/mapper/vg-root /var/lib/postgresql ext4 rw 0 0\n" +
		"/dev/nvme0n1p1 /data ext4 rw 0 0\n" +
		"overlay /srv/overlay overlay rw,upperdir=/data/docker/upper,workdir=/data/docker/work 0 0\n" +
		"tmpfs /srv/tmp tmpfs rw 0 0\n" +
		"nfs.example:/export /srv/nfs nfs4 rw 0 0\n"

	cases := []struct {
		path string
		want string
	}{
		{"/var/lib/postgresql/17/main", "hdd"}, // LVM -> dm-0 -> sda (rotational)
		{"/data/pg", "ssd"},                    // nvme partition
		{"/srv/overlay/merged", "ssd"},         // overlay upperdir -> /data
		{"/srv/tmp/pg", "unknown"},             // tmpfs
		{"/srv/nfs/pg", "unknown"},             // network filesystem
	}
	for _, c := range cases {
		if got := detectDiskTypeWith(fs, mounts, c.path); got != c.want {
			t.Errorf("detectDiskTypeWith(%q) = %q, want %q", c.path, got, c.want)
		}
	}

	// A dm device spanning SSD and HDD leaves is unknown, not SSD.
	if got := storageClassForDevice(fs, "dm-1"); got != "unknown" {
		t.Errorf("mixed dm device = %q, want unknown", got)
	}
}

func TestScanAllDevicesSkipsVirtual(t *testing.T) {
	fs := fakeSysFS{
		files: map[string]string{
			"block/sda/queue/rotational":   "0\n",
			"block/loop0/queue/rotational": "1\n",
			"block/ram0/queue/rotational":  "1\n",
		},
		dirs: map[string][]string{
			"block": {"sda", "loop0", "ram0"},
		},
	}
	if got := scanAllDevices(fs); got != "ssd" {
		t.Errorf("scanAllDevices = %q, want ssd (virtual devices ignored)", got)
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
