package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const reportWidth = 64

func writeReport(results []BuildResult, at time.Time, path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}

	var sb strings.Builder
	rule := strings.Repeat("=", reportWidth)

	sb.WriteString(rule + "\n")
	sb.WriteString(centered("PUNCHCARD SECURITY HEALTH REPORT", reportWidth) + "\n")
	sb.WriteString(centered(at.Format("Monday, 2006-01-02"), reportWidth) + "\n")
	sb.WriteString(rule + "\n\n")

	clean, flagged, notScanned := 0, 0, 0
	var totalDur time.Duration

	for _, r := range results {
		totalDur += r.Duration

		switch {
		case !r.Scanned:
			notScanned++
			sb.WriteString(fmt.Sprintf("  [-------]  %-36s  (%s)\n", r.Project, r.ErrMsg))

		case r.Clean:
			clean++
			sb.WriteString(fmt.Sprintf("  [CLEAN  ]  %-36s  %s\n", r.Project, fmtDur(r.Duration)))

		default:
			flagged++
			sb.WriteString(fmt.Sprintf("  [FLAGGED]  %-36s  %s\n", r.Project, fmtDur(r.Duration)))
			for _, line := range r.Output {
				sb.WriteString(fmt.Sprintf("             | %s\n", line))
			}
			sb.WriteString("\n")
		}
	}

	sb.WriteString(strings.Repeat("-", reportWidth) + "\n")

	n := len(results)
	noun := "project"
	if n != 1 {
		noun = "projects"
	}
	sb.WriteString(fmt.Sprintf("  %d %s  |  %d clean  |  %d flagged  |  %d not scanned  |  %s\n",
		n, noun, clean, flagged, notScanned, fmtDur(totalDur)))
	sb.WriteString(rule + "\n")

	return os.WriteFile(path, []byte(sb.String()), 0644)
}

func centered(s string, width int) string {
	if len(s) >= width {
		return s
	}
	pad := (width - len(s)) / 2
	return strings.Repeat(" ", pad) + s
}

func fmtDur(d time.Duration) string {
	if d < time.Second {
		return fmt.Sprintf("%dms", d.Milliseconds())
	}
	return fmt.Sprintf("%.1fs", d.Seconds())
}
