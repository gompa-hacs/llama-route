//go:build unix && !darwin

package perf

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/mostlygeek/llama-swap/internal/logmon"
)

const (
	amdVendorID         = "0x1002"
	sysfsDRMPathDefault = "/sys/class/drm"
)

// Overridable for tests.
var sysfsDRMPath = sysfsDRMPathDefault

func trySysfs(ctx context.Context, every time.Duration, logger *logmon.Monitor) (chan []GpuStat, error) {
	stats, err := readSysfs()
	if err != nil {
		return nil, err
	}
	if len(stats) == 0 {
		return nil, ErrNoGpuTool
	}

	if every < time.Second {
		every = time.Second
	}

	ch := make(chan []GpuStat, 1)

	go func() {
		defer close(ch)
		ticker := time.NewTicker(every)
		defer ticker.Stop()

		// Emit the discovery sample immediately so callers don't wait a full tick.
		select {
		case ch <- stats:
		default:
		}

		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				stats, err := readSysfs()
				if err != nil || len(stats) == 0 {
					if err != nil {
						logger.Debugf("sysfs read: %s", err.Error())
					}
					continue
				}
				select {
				case ch <- stats:
				default:
				}
			}
		}
	}()

	return ch, nil
}

func readSysfs() ([]GpuStat, error) {
	return readSysfsFrom(sysfsDRMPath)
}

func readSysfsFrom(drmPath string) ([]GpuStat, error) {
	entries, err := os.ReadDir(drmPath)
	if err != nil {
		return nil, err
	}

	stats := make([]GpuStat, 0)
	for _, entry := range entries {
		name := entry.Name()
		if !isDRMCardDir(name) {
			continue
		}
		stat, ok := readAmdgpuCard(filepath.Join(drmPath, name), name)
		if !ok {
			continue
		}
		stats = append(stats, stat)
	}

	sort.Slice(stats, func(i, j int) bool { return stats[i].ID < stats[j].ID })
	return stats, nil
}

func isDRMCardDir(name string) bool {
	if !strings.HasPrefix(name, "card") {
		return false
	}
	rest := name[len("card"):]
	if rest == "" {
		return false
	}
	for _, c := range rest {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

func readAmdgpuCard(cardPath, cardName string) (GpuStat, bool) {
	devicePath := filepath.Join(cardPath, "device")

	vendor, _ := readSysfsTrimmed(filepath.Join(devicePath, "vendor"))
	_, hasVRAM := readSysfsUint(filepath.Join(devicePath, "mem_info_vram_total"))
	_, hasBusy := readSysfsUint(filepath.Join(devicePath, "gpu_busy_percent"))
	if vendor != amdVendorID && !hasVRAM && !hasBusy {
		return GpuStat{}, false
	}
	// Require at least one amdgpu-specific metric so we don't claim random DRM devices.
	if !hasVRAM && !hasBusy {
		return GpuStat{}, false
	}

	id, err := strconv.Atoi(strings.TrimPrefix(cardName, "card"))
	if err != nil {
		return GpuStat{}, false
	}

	stat := GpuStat{
		Timestamp: time.Now(),
		ID:        id,
		Name:      amdgpuDisplayName(devicePath, cardName),
		UUID:      amdgpuUUID(devicePath),
	}

	if busy, ok := readSysfsUint(filepath.Join(devicePath, "gpu_busy_percent")); ok {
		stat.GpuUtilPct = float64(busy)
	}

	const toMB = 1024 * 1024
	if total, ok := readSysfsUint(filepath.Join(devicePath, "mem_info_vram_total")); ok {
		stat.MemTotalMB = int(total / toMB)
	}
	if used, ok := readSysfsUint(filepath.Join(devicePath, "mem_info_vram_used")); ok {
		stat.MemUsedMB = int(used / toMB)
	}
	if memBusy, ok := readSysfsUint(filepath.Join(devicePath, "mem_busy_percent")); ok {
		stat.MemUtilPct = float64(memBusy)
	} else if stat.MemTotalMB > 0 {
		stat.MemUtilPct = float64(stat.MemUsedMB) / float64(stat.MemTotalMB) * 100
	}

	readAmdgpuHwmon(devicePath, &stat)
	readAmdgpuClocks(devicePath, &stat)

	return stat, true
}

func amdgpuUUID(devicePath string) string {
	uevent, err := os.Open(filepath.Join(devicePath, "uevent"))
	if err != nil {
		return ""
	}
	defer uevent.Close()

	scanner := bufio.NewScanner(uevent)
	for scanner.Scan() {
		line := scanner.Text()
		if after, ok := strings.CutPrefix(line, "PCI_SLOT_NAME="); ok {
			return after
		}
	}
	return ""
}

func amdgpuDisplayName(devicePath, cardName string) string {
	if slot := amdgpuUUID(devicePath); slot != "" {
		if name := lspciDeviceName(slot); name != "" {
			return name
		}
	}
	if vendorDevice := pciIDPair(devicePath); vendorDevice != "" {
		return fmt.Sprintf("AMD GPU %s (%s)", vendorDevice, cardName)
	}
	return fmt.Sprintf("AMD GPU (%s)", cardName)
}

func pciIDPair(devicePath string) string {
	vendor, ok := readSysfsTrimmed(filepath.Join(devicePath, "vendor"))
	if !ok {
		return ""
	}
	device, ok := readSysfsTrimmed(filepath.Join(devicePath, "device"))
	if !ok {
		return ""
	}
	return strings.TrimPrefix(vendor, "0x") + ":" + strings.TrimPrefix(device, "0x")
}

func lspciDeviceName(pciSlot string) string {
	if _, err := exec.LookPath("lspci"); err != nil {
		return ""
	}
	out, err := exec.Command("lspci", "-s", pciSlot, "-nn").Output()
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(out))
	if line == "" {
		return ""
	}
	// Example: 06:00.0 VGA compatible controller [0300]: Advanced Micro Devices, Inc. [AMD/ATI] Rembrandt [Radeon 680M] [1002:1681] (rev 0b)
	if idx := strings.Index(line, ": "); idx >= 0 && idx+2 < len(line) {
		name := line[idx+2:]
		if end := strings.LastIndex(name, " ["); end > 0 {
			name = name[:end]
		}
		name = strings.TrimSpace(name)
		if name != "" {
			return name
		}
	}
	return ""
}

func readAmdgpuHwmon(devicePath string, stat *GpuStat) {
	hwmonRoot := filepath.Join(devicePath, "hwmon")
	entries, err := os.ReadDir(hwmonRoot)
	if err != nil {
		return
	}

	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "hwmon") {
			continue
		}
		hwmonPath := filepath.Join(hwmonRoot, entry.Name())
		name, _ := readSysfsTrimmed(filepath.Join(hwmonPath, "name"))
		if name != "" && name != "amdgpu" {
			continue
		}

		temps := readHwmonTemps(hwmonPath)
		if edge, ok := temps["edge"]; ok {
			stat.TempC = edge
		} else if junction, ok := temps["junction"]; ok {
			stat.TempC = junction
		} else {
			for _, t := range temps {
				stat.TempC = t
				break
			}
		}
		if mem, ok := temps["mem"]; ok {
			stat.VramTempC = mem
		}

		if avg, ok := readSysfsUint(filepath.Join(hwmonPath, "power1_average")); ok && avg > 0 {
			stat.PowerDrawW = float64(avg) / 1_000_000
		} else if input, ok := readSysfsUint(filepath.Join(hwmonPath, "power1_input")); ok {
			stat.PowerDrawW = float64(input) / 1_000_000
		}

		if pwm, ok := readSysfsUint(filepath.Join(hwmonPath, "pwm1")); ok {
			stat.FanSpeedPct = float64(pwm) / 255.0 * 100.0
		} else if rpm, ok := readSysfsUint(filepath.Join(hwmonPath, "fan1_input")); ok {
			if maxRPM, ok := readSysfsUint(filepath.Join(hwmonPath, "fan1_max")); ok && maxRPM > 0 {
				stat.FanSpeedPct = float64(rpm) / float64(maxRPM) * 100.0
			}
		}

		freqs := readHwmonFreqsMHz(hwmonPath)
		if sclk, ok := freqs["sclk"]; ok {
			stat.ClockMHz = sclk
		}
		if mclk, ok := freqs["mclk"]; ok {
			stat.MemClockMHz = mclk
		}

		// First matching amdgpu hwmon is enough.
		return
	}
}

// readAmdgpuClocks fills ClockMHz / MemClockMHz from pp_dpm_* when hwmon
// did not expose frequencies (common on some Polaris cards).
func readAmdgpuClocks(devicePath string, stat *GpuStat) {
	if stat.ClockMHz == 0 {
		if mhz, ok := parsePPDPMCurrentMHz(filepath.Join(devicePath, "pp_dpm_sclk")); ok {
			stat.ClockMHz = mhz
		}
	}
	if stat.MemClockMHz == 0 {
		if mhz, ok := parsePPDPMCurrentMHz(filepath.Join(devicePath, "pp_dpm_mclk")); ok {
			stat.MemClockMHz = mhz
		}
	}
}

// parsePPDPMCurrentMHz reads amdgpu pp_dpm_{s,m}clk output and returns the
// MHz value marked with '*'. Example line: "1: 1360Mhz *"
func parsePPDPMCurrentMHz(path string) (int, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, false
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.Contains(line, "*") {
			continue
		}
		// "1: 1360Mhz *" or "1: 1360Mhz*"
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		rest := strings.TrimSpace(parts[1])
		rest = strings.TrimSuffix(rest, "*")
		rest = strings.TrimSpace(rest)
		rest = strings.TrimSuffix(strings.ToLower(rest), "mhz")
		rest = strings.TrimSpace(rest)
		mhz, err := strconv.Atoi(rest)
		if err != nil {
			continue
		}
		return mhz, true
	}
	return 0, false
}

func readHwmonFreqsMHz(hwmonPath string) map[string]int {
	freqs := make(map[string]int)
	for i := 1; i <= 10; i++ {
		labelPath := filepath.Join(hwmonPath, fmt.Sprintf("freq%d_label", i))
		inputPath := filepath.Join(hwmonPath, fmt.Sprintf("freq%d_input", i))
		hz, ok := readSysfsUint(inputPath)
		if !ok {
			continue
		}
		label, ok := readSysfsTrimmed(labelPath)
		if !ok {
			label = fmt.Sprintf("freq%d", i)
		}
		freqs[strings.ToLower(label)] = int(hz / 1_000_000)
	}
	return freqs
}

func readHwmonTemps(hwmonPath string) map[string]int {
	temps := make(map[string]int)
	for i := 1; i <= 10; i++ {
		labelPath := filepath.Join(hwmonPath, fmt.Sprintf("temp%d_label", i))
		inputPath := filepath.Join(hwmonPath, fmt.Sprintf("temp%d_input", i))
		milliC, ok := readSysfsUint(inputPath)
		if !ok {
			continue
		}
		label, ok := readSysfsTrimmed(labelPath)
		if !ok {
			label = fmt.Sprintf("temp%d", i)
		}
		temps[strings.ToLower(label)] = int(milliC / 1000)
	}
	return temps
}

func readSysfsTrimmed(path string) (string, bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(data)), true
}

func readSysfsUint(path string) (uint64, bool) {
	s, ok := readSysfsTrimmed(path)
	if !ok {
		return 0, false
	}
	// Some amdgpu nodes append units or trailing junk; take the leading integer.
	if i := strings.IndexAny(s, " \t\n"); i >= 0 {
		s = s[:i]
	}
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, false
	}
	return v, true
}
