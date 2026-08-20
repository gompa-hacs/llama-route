//go:build unix && !darwin

package perf

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func writeSysfsFile(t *testing.T, path, contents string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o755))
	require.NoError(t, os.WriteFile(path, []byte(contents+"\n"), 0o644))
}

func TestIsDRMCardDir(t *testing.T) {
	assert.True(t, isDRMCardDir("card0"))
	assert.True(t, isDRMCardDir("card12"))
	assert.False(t, isDRMCardDir("card1-DP-1"))
	assert.False(t, isDRMCardDir("renderD128"))
	assert.False(t, isDRMCardDir("card"))
}

func TestReadSysfsFrom_RX580Style(t *testing.T) {
	root := t.TempDir()

	// Active card with full sensors (matches crapboxv2 card1).
	active := filepath.Join(root, "card1", "device")
	writeSysfsFile(t, filepath.Join(active, "vendor"), "0x1002")
	writeSysfsFile(t, filepath.Join(active, "device"), "0x67df")
	writeSysfsFile(t, filepath.Join(active, "uevent"), "DRIVER=amdgpu\nPCI_SLOT_NAME=0000:ff:1f.7\n")
	writeSysfsFile(t, filepath.Join(active, "gpu_busy_percent"), "100")
	writeSysfsFile(t, filepath.Join(active, "mem_busy_percent"), "37")
	writeSysfsFile(t, filepath.Join(active, "mem_info_vram_total"), "8589934592")
	writeSysfsFile(t, filepath.Join(active, "mem_info_vram_used"), "1908224000")
	hwmon := filepath.Join(active, "hwmon", "hwmon3")
	writeSysfsFile(t, filepath.Join(hwmon, "name"), "amdgpu")
	writeSysfsFile(t, filepath.Join(hwmon, "temp1_label"), "edge")
	writeSysfsFile(t, filepath.Join(hwmon, "temp1_input"), "40000")
	writeSysfsFile(t, filepath.Join(hwmon, "power1_label"), "PPT")
	writeSysfsFile(t, filepath.Join(hwmon, "power1_input"), "158738000")
	writeSysfsFile(t, filepath.Join(hwmon, "pwm1"), "128")
	writeSysfsFile(t, filepath.Join(hwmon, "fan1_max"), "3800")
	writeSysfsFile(t, filepath.Join(hwmon, "freq1_label"), "sclk")
	writeSysfsFile(t, filepath.Join(hwmon, "freq1_input"), "1360000000")
	writeSysfsFile(t, filepath.Join(hwmon, "freq2_label"), "mclk")
	writeSysfsFile(t, filepath.Join(hwmon, "freq2_input"), "2000000000")
	writeSysfsFile(t, filepath.Join(active, "pp_dpm_sclk"), "0: 300Mhz\n1: 1360Mhz *\n")
	writeSysfsFile(t, filepath.Join(active, "pp_dpm_mclk"), "0: 300Mhz\n1: 2000Mhz *\n")

	// Idle secondary card: VRAM only, sensors unavailable (EBUSY / missing busy %).
	idle := filepath.Join(root, "card3", "device")
	writeSysfsFile(t, filepath.Join(idle, "vendor"), "0x1002")
	writeSysfsFile(t, filepath.Join(idle, "device"), "0x67df")
	writeSysfsFile(t, filepath.Join(idle, "uevent"), "DRIVER=amdgpu\nPCI_SLOT_NAME=0000:fe:1e.6\n")
	writeSysfsFile(t, filepath.Join(idle, "mem_info_vram_total"), "8589934592")
	writeSysfsFile(t, filepath.Join(idle, "mem_info_vram_used"), "5439488")
	idleHwmon := filepath.Join(idle, "hwmon", "hwmon4")
	writeSysfsFile(t, filepath.Join(idleHwmon, "name"), "amdgpu")
	writeSysfsFile(t, filepath.Join(idleHwmon, "temp1_label"), "edge")
	// No temp1_input / power / fan files → simulates Device or resource busy.

	// Connector symlink name and NVIDIA card must be ignored.
	writeSysfsFile(t, filepath.Join(root, "card1-DP-1", "device", "vendor"), "0x1002")
	nvidia := filepath.Join(root, "card2", "device")
	writeSysfsFile(t, filepath.Join(nvidia, "vendor"), "0x10de")
	writeSysfsFile(t, filepath.Join(nvidia, "device"), "0x25ad")

	stats, err := readSysfsFrom(root)
	require.NoError(t, err)
	require.Len(t, stats, 2)

	assert.Equal(t, 1, stats[0].ID)
	assert.Equal(t, "0000:ff:1f.7", stats[0].UUID)
	assert.Equal(t, 100.0, stats[0].GpuUtilPct)
	assert.Equal(t, 37.0, stats[0].MemUtilPct)
	assert.Equal(t, 1819, stats[0].MemUsedMB)  // 1908224000 / 1MiB
	assert.Equal(t, 8192, stats[0].MemTotalMB) // 8GiB
	assert.Equal(t, 40, stats[0].TempC)
	assert.InDelta(t, 158.738, stats[0].PowerDrawW, 0.001)
	assert.InDelta(t, 50.196, stats[0].FanSpeedPct, 0.1) // 128/255
	assert.Equal(t, 1360, stats[0].ClockMHz)
	assert.Equal(t, 2000, stats[0].MemClockMHz)
	assert.Equal(t, "AMD GPU 1002:67df (card1)", stats[0].Name)

	assert.Equal(t, 3, stats[1].ID)
	assert.Equal(t, "0000:fe:1e.6", stats[1].UUID)
	assert.Equal(t, 0.0, stats[1].GpuUtilPct)
	assert.Equal(t, 8192, stats[1].MemTotalMB)
	assert.Equal(t, 5, stats[1].MemUsedMB)
	assert.InDelta(t, float64(5)/float64(8192)*100, stats[1].MemUtilPct, 0.01)
	assert.Equal(t, 0, stats[1].TempC)
	assert.Equal(t, 0.0, stats[1].PowerDrawW)
}

func TestReadSysfsFrom_NoAMD(t *testing.T) {
	root := t.TempDir()
	nvidia := filepath.Join(root, "card0", "device")
	writeSysfsFile(t, filepath.Join(nvidia, "vendor"), "0x10de")
	writeSysfsFile(t, filepath.Join(nvidia, "device"), "0x25ad")

	stats, err := readSysfsFrom(root)
	require.NoError(t, err)
	assert.Empty(t, stats)
}
