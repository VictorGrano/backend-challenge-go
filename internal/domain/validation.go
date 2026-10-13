package domain

import (
	"strings"
	"time"
)

func blank(s string) bool {
	return strings.TrimSpace(s) == ""
}

func laterOf(current, now time.Time) time.Time {
	now = now.UTC()
	if now.Before(current) {
		return current
	}

	return now
}
