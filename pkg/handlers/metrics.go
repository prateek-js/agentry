package handlers

import (
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/agentry/agentry/pkg/models"
)

// MetricsHandler returns system metrics (CPU, memory, disk).
func MetricsHandler(w http.ResponseWriter, r *http.Request) {
	cpu := models.CPUInfo{Cores: runtime.NumCPU()}

	// Parse /proc/loadavg for load averages.
	if data, err := os.ReadFile("/proc/loadavg"); err == nil {
		fields := strings.Fields(string(data))
		if len(fields) >= 2 {
			cpu.Load1m, _ = strconv.ParseFloat(fields[0], 64)
			cpu.Load5m, _ = strconv.ParseFloat(fields[1], 64)
		}
	}

	// Parse /proc/meminfo for memory.
	mem := models.MemoryInfo{}
	if data, err := os.ReadFile("/proc/meminfo"); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.Fields(line)
			if len(parts) < 2 {
				continue
			}
			val, _ := strconv.ParseFloat(parts[1], 64)
			valMB := val / 1024.0
			switch parts[0] {
			case "MemTotal:":
				mem.TotalMB = valMB
			case "MemAvailable:":
				mem.AvailableMB = valMB
			}
		}
		mem.UsedMB = mem.TotalMB - mem.AvailableMB
		if mem.TotalMB > 0 {
			mem.PctUsed = (mem.UsedMB / mem.TotalMB) * 100
		}
	}

	// Disk usage via syscall.
	disk := models.DiskInfo{}
	var stat syscall.Statfs_t
	if err := syscall.Statfs("/", &stat); err == nil {
		disk.TotalGB = float64(stat.Blocks*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
		disk.AvailableGB = float64(stat.Bavail*uint64(stat.Bsize)) / (1024 * 1024 * 1024)
		disk.UsedGB = disk.TotalGB - disk.AvailableGB
		if disk.TotalGB > 0 {
			disk.PctUsed = (disk.UsedGB / disk.TotalGB) * 100
		}
	}

	Success(w, "metrics", map[string]interface{}{
		"cpu":    cpu,
		"memory": mem,
		"disk":   disk,
	})
}
