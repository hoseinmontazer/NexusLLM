package handlers

import "fmt"

// ─── shared helpers used across multiple handlers ─────────────────────────────

func orDefaultStr(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func orDefaultInt(v, def int) int {
	if v == 0 {
		return def
	}
	return v
}

func containerCPUStr(cores int) string {
	if cores <= 0 {
		return ""
	}
	return fmt.Sprintf("%d", cores)
}

func containerMemStr(mb int64) string {
	if mb <= 0 {
		return ""
	}
	return fmt.Sprintf("%dm", mb)
}
