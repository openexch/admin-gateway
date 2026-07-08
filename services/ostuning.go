// SPDX-License-Identifier: Apache-2.0
package services

import (
	"os"
	"path/filepath"

	"github.com/match/admin-gateway/config"
	"github.com/match/admin-gateway/logging"
)

// ApplyProfileOSKnobs best-effort applies the CPU governor and THP mode for the
// active profile. Both are runtime-writable sysfs files but normally need root,
// so failures are logged and ignored — reboot-persistence stays with
// openexchange-tuning.service, and this only nudges the live values.
//
// It deliberately does NOT touch socket rmem/wmem: unpersisted socket limits
// crash-looped the drivers and corrupted an archive once (match#48), so those
// stay boot-persistent only.
func ApplyProfileOSKnobs(prof config.Profile) {
	log := logging.Component("ostuning")

	if prof.Governor != "" {
		matches, _ := filepath.Glob("/sys/devices/system/cpu/cpufreq/policy*/scaling_governor")
		applied := 0
		for _, f := range matches {
			if err := os.WriteFile(f, []byte(prof.Governor), 0o644); err == nil {
				applied++
			}
		}
		switch {
		case len(matches) == 0:
			log.Warn("cpu governor: no cpufreq policies present", "governor", prof.Governor)
		case applied < len(matches):
			log.Warn("cpu governor: partial write (insufficient privileges?)",
				"governor", prof.Governor, "applied", applied, "total", len(matches))
		default:
			log.Info("cpu governor applied", "governor", prof.Governor, "policies", len(matches))
		}
	}

	if prof.THP != "" {
		const thpFile = "/sys/kernel/mm/transparent_hugepage/enabled"
		if err := os.WriteFile(thpFile, []byte(prof.THP), 0o644); err != nil {
			log.Warn("thp: write failed (insufficient privileges?)", "thp", prof.THP, "err", err)
		} else {
			log.Info("thp applied", "thp", prof.THP)
		}
	}
}
