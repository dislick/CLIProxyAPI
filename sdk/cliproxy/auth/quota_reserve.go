package auth

import (
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// MetadataQuotaReservePercent reserves part of a Codex account's allowance for
// use outside the proxy. It is stored in the credential's auth-file metadata.
const MetadataQuotaReservePercent = "quota_reserve_percent"

// AuthQuotaReservePercent reads and validates a per-credential reserve.
// Missing, null and zero values disable the reserve.
func AuthQuotaReservePercent(auth *Auth) (float64, error) {
	if auth == nil || auth.Metadata[MetadataQuotaReservePercent] == nil {
		return 0, nil
	}
	reserve, errParse := strconv.ParseFloat(strings.TrimSpace(fmt.Sprint(auth.Metadata[MetadataQuotaReservePercent])), 64)
	if errParse != nil || math.IsNaN(reserve) || math.IsInf(reserve, 0) || reserve < 0 || reserve > 100 {
		return 0, fmt.Errorf("%s must be a number between 0 and 100", MetadataQuotaReservePercent)
	}
	if reserve > 0 && !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
		return 0, fmt.Errorf("%s is supported only for Codex credentials", MetadataQuotaReservePercent)
	}
	return reserve, nil
}

func validateAuthRoutingMetadata(auth *Auth) error {
	if errWeight := ValidateAuthWeight(auth); errWeight != nil {
		return errWeight
	}
	_, errReserve := AuthQuotaReservePercent(auth)
	return errReserve
}

// codexQuotaReserveBlocks excludes a credential when any current base window
// reaches its reserve. Expired windows stop blocking without changing the last
// observation, allowing a real request to learn the new window after reset.
func codexQuotaReserveBlocks(auth *Auth, now time.Time) (time.Time, bool) {
	reserve, errReserve := AuthQuotaReservePercent(auth)
	if errReserve != nil || reserve <= 0 {
		return time.Time{}, false
	}
	if reserve == 100 {
		return time.Time{}, true
	}
	var recoverAt time.Time
	blocked := false
	for _, prefix := range codexQuotaWindowPrefixes {
		used, errUsed := strconv.ParseFloat(strings.TrimSpace(auth.Quota.Signals[prefix+"Used-Percent"]), 64)
		if errUsed != nil || math.IsNaN(used) || math.IsInf(used, 0) || used < 100-reserve {
			continue
		}
		reset, known := codexWindowResetAt(auth.Quota.Signals, prefix, auth.Quota.ObservedAt)
		if known && !reset.After(now) {
			continue
		}
		// A known exhausted allowance without a reset stays protected until a
		// fresh observation or settings change supplies a recovery deadline.
		if !known {
			return time.Time{}, true
		}
		blocked = true
		if reset.After(recoverAt) {
			recoverAt = reset
		}
	}
	return recoverAt, blocked
}
