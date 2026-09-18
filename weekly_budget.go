package main

import (
	"fmt"
	"math"
	"time"
)

const (
	weeklyBudgetSampleLimit       = 14
	weeklyBudgetMinimumFullPeriod = 20 * time.Hour
	weeklyBudgetResetAdvance      = 24 * time.Hour
	weeklyBudgetResetJumpPercent  = 20.0
	weeklyBudgetResetNearFull     = 95.0
)

func (g *quotaGuard) updateWeeklyBudgetLocked(account *accountState, snap quotaWindowSnapshot, now time.Time) {
	if account == nil || !g.cfg.WeeklyBudgetEnabled || snap.At.IsZero() || snap.ResetAt == nil || snap.ResetAt.IsZero() {
		return
	}
	if g.cfg.QuotaSnapshotMaxAgeSecs > 0 && now.Sub(snap.At) > time.Duration(g.cfg.QuotaSnapshotMaxAgeSecs)*time.Second {
		return
	}
	observedAt := snap.At
	observedResetAt := *snap.ResetAt
	state := &account.WeeklyBudget
	dayStart := weeklyBudgetDayStart(observedAt, g.cfg.WeeklyBudgetTimezoneOffsetHours)
	localDate := weeklyBudgetLocalDate(dayStart, g.cfg.WeeklyBudgetTimezoneOffsetHours)
	cycleChangeReason := weeklyBudgetCycleChangeReason(*state, observedAt, observedResetAt, snap.RemainingPercent)
	if cycleChangeReason != "" {
		*state = weeklyBudgetState{
			CycleResetAt:         observedResetAt,
			LastObservedResetAt:  observedResetAt,
			AdjustmentMultiplier: 1,
			Reason:               cycleChangeReason,
		}
	}
	state.LastObservedResetAt = observedResetAt
	state.ResetDriftSeconds = int64(observedResetAt.Sub(state.CycleResetAt).Seconds())
	resetAt := state.CycleResetAt

	expectedRemaining := weeklyExpectedRemaining(dayStart, resetAt, g.cfg.MinRemainingPercent)
	state.ExpectedRemaining = round2(expectedRemaining)
	state.DeviationPercent = round2(snap.RemainingPercent - expectedRemaining)
	state.LastSampleAt = observedAt

	sample := weeklyQuotaSample{LocalDate: localDate, At: dayStart, ObservedAt: observedAt, RemainingPercent: snap.RemainingPercent, ResetAt: resetAt}
	if len(state.Samples) == 0 {
		state.Samples = append(state.Samples, sample)
		return
	}
	last := state.Samples[len(state.Samples)-1]
	if last.LocalDate == localDate {
		if last.ObservedAt.IsZero() {
			last.ObservedAt = last.At
		}
		last.At = weeklyBudgetDateStart(last.LocalDate, g.cfg.WeeklyBudgetTimezoneOffsetHours, last.At)
		state.Samples[len(state.Samples)-1] = last
		return
	}
	last.At = weeklyBudgetDateStart(last.LocalDate, g.cfg.WeeklyBudgetTimezoneOffsetHours, last.At)

	lastObservedAt := last.ObservedAt
	if lastObservedAt.IsZero() {
		lastObservedAt = last.At
	}
	daysRemaining := resetAt.Sub(lastObservedAt).Hours() / 24
	if daysRemaining < 1 {
		daysRemaining = 1
	}
	elapsed := sample.ObservedAt.Sub(lastObservedAt)
	if elapsed < weeklyBudgetMinimumFullPeriod {
		state.AdjustmentMultiplier = 1
		state.Reason = fmt.Sprintf("T+1 waiting for full UTC+8 day: observed interval %.1fh", elapsed.Hours())
		state.Samples = append(state.Samples, sample)
		if len(state.Samples) > weeklyBudgetSampleLimit {
			state.Samples = append([]weeklyQuotaSample(nil), state.Samples[len(state.Samples)-weeklyBudgetSampleLimit:]...)
		}
		return
	}
	elapsedDays := elapsed.Hours() / 24
	if elapsedDays <= 0 {
		return
	}
	actualBurn := math.Max(0, last.RemainingPercent-snap.RemainingPercent)
	expectedBurn := math.Max(0, last.RemainingPercent-g.cfg.MinRemainingPercent) / daysRemaining * elapsedDays
	state.LastDailyBurnPercent = round2(actualBurn)
	state.ExpectedDailyBurn = round2(expectedBurn)

	maxAdjustment := g.cfg.WeeklyBudgetMaxAdjustmentPercent / 100
	trajectoryCorrection := 0.7 * (snap.RemainingPercent - expectedRemaining) / 100
	burnCorrection := 0.3 * (expectedBurn - actualBurn) / 100
	correction := math.Max(-maxAdjustment, math.Min(maxAdjustment, trajectoryCorrection+burnCorrection))
	state.AdjustmentMultiplier = round4(1 + correction)
	state.Reason = fmt.Sprintf("T+1 weekly budget: burn %.2f%% vs %.2f%% expected, remaining %.2f%% vs %.2f%% trajectory", actualBurn, expectedBurn, snap.RemainingPercent, expectedRemaining)
	state.Samples = append(state.Samples, sample)
	if len(state.Samples) > weeklyBudgetSampleLimit {
		state.Samples = append([]weeklyQuotaSample(nil), state.Samples[len(state.Samples)-weeklyBudgetSampleLimit:]...)
	}
}

func weeklyBudgetCycleChangeReason(state weeklyBudgetState, observedAt, observedResetAt time.Time, remainingPercent float64) string {
	if state.CycleResetAt.IsZero() {
		return "new weekly cycle baseline"
	}
	if !observedResetAt.After(state.CycleResetAt.Add(weeklyBudgetResetAdvance)) {
		return ""
	}
	if !observedAt.Before(state.CycleResetAt) {
		return "new weekly cycle baseline"
	}

	// Upstream can start a new quota cycle before the previously reported
	// boundary. Treat a materially advanced reset as real only when quota also
	// refills, so reset_at drift alone cannot discard the current baseline.
	previousRemaining, hasPrevious := weeklyBudgetLastRemaining(state)
	if remainingPercent >= weeklyBudgetResetNearFull ||
		(hasPrevious && remainingPercent-previousRemaining >= weeklyBudgetResetJumpPercent) {
		return "early weekly cycle reset baseline"
	}
	return ""
}

func weeklyBudgetLastRemaining(state weeklyBudgetState) (float64, bool) {
	if len(state.Samples) == 0 {
		return 0, false
	}
	return state.Samples[len(state.Samples)-1].RemainingPercent, true
}

func (g *quotaGuard) weeklyBudgetMultiplierLocked(account *accountState) float64 {
	if account == nil || !g.cfg.WeeklyBudgetEnabled {
		return 1
	}
	multiplier := account.WeeklyBudget.AdjustmentMultiplier
	if multiplier <= 0 {
		return 1
	}
	maxAdjustment := g.cfg.WeeklyBudgetMaxAdjustmentPercent / 100
	return math.Max(1-maxAdjustment, math.Min(1+maxAdjustment, multiplier))
}

func weeklyBudgetDisplayReason(state weeklyBudgetState) string {
	if state.ResetDriftSeconds == 0 {
		return state.Reason
	}
	return fmt.Sprintf("%s; reset drift %+.0fm ignored", state.Reason, float64(state.ResetDriftSeconds)/60)
}

func weeklyBudgetLocalDate(now time.Time, offsetHours int) string {
	location := weeklyBudgetLocation(offsetHours)
	return now.In(location).Format("2006-01-02")
}

func weeklyBudgetDayStart(now time.Time, offsetHours int) time.Time {
	local := now.In(weeklyBudgetLocation(offsetHours))
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, local.Location()).UTC()
}

func weeklyBudgetDateStart(localDate string, offsetHours int, fallback time.Time) time.Time {
	parsed, errParse := time.ParseInLocation("2006-01-02", localDate, weeklyBudgetLocation(offsetHours))
	if errParse != nil {
		return fallback
	}
	return parsed.UTC()
}

func weeklyBudgetLocation(offsetHours int) *time.Location {
	return time.FixedZone("weekly-budget", offsetHours*60*60)
}

func weeklyExpectedRemaining(now, resetAt time.Time, reserve float64) float64 {
	cycleStart := resetAt.Add(-7 * 24 * time.Hour)
	if !now.After(cycleStart) {
		return 100
	}
	if !now.Before(resetAt) {
		return reserve
	}
	remainingFraction := resetAt.Sub(now).Seconds() / (7 * 24 * time.Hour).Seconds()
	return reserve + (100-reserve)*remainingFraction
}

func round4(value float64) float64 {
	return math.Round(value*10000) / 10000
}
