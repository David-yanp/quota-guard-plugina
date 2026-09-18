package main

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

const healthObservationRetention = 72 * time.Hour

func healthObservationKey(model, apiKeyID string) string {
	model = strings.TrimSpace(model)
	if model == "" {
		model = "unknown"
	}
	apiKeyID = strings.TrimSpace(apiKeyID)
	if apiKeyID == "" {
		apiKeyID = "unknown"
	}
	return model + "\x00" + apiKeyID
}

func healthObservationParts(key string) (string, string) {
	parts := strings.SplitN(key, "\x00", 2)
	if len(parts) == 1 {
		return parts[0], ""
	}
	return parts[0], parts[1]
}

func safeUsageAPIKeyID(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return ""
		}
	}
	return value
}

func healthBucketStart(at time.Time) time.Time {
	return at.UTC().Truncate(time.Minute)
}

func (g *quotaGuard) ensureHealthObservationLocked(account *accountState, model, apiKeyID string) *modelHealthObservation {
	if account.HealthObservations == nil {
		account.HealthObservations = map[string]*modelHealthObservation{}
	}
	key := healthObservationKey(model, apiKeyID)
	observation := account.HealthObservations[key]
	if observation == nil {
		observation = &modelHealthObservation{Model: strings.TrimSpace(model), APIKeyID: strings.TrimSpace(apiKeyID)}
		if observation.Model == "" {
			observation.Model = "unknown"
		}
		account.HealthObservations[key] = observation
	}
	return observation
}

func (g *quotaGuard) recordHealthPickLocked(account *accountState, model, apiKeyID string, now time.Time) {
	if account == nil {
		return
	}
	observation := g.ensureHealthObservationLocked(account, model, apiKeyID)
	observation.CurrentInflight++
	if observation.CurrentInflight > observation.MaxInflight {
		observation.MaxInflight = observation.CurrentInflight
	}
	if now.After(observation.LastRequestAt) {
		observation.LastRequestAt = now
	}
	bucket := g.ensureHealthBucketLocked(observation, now)
	if observation.CurrentInflight > bucket.MaxInflight {
		bucket.MaxInflight = observation.CurrentInflight
	}
}

func (g *quotaGuard) recordHealthCompletionLocked(account *accountState, reserve inflightReserve, rec pluginapi.UsageRecord, at time.Time) {
	if account == nil {
		return
	}
	apiKeyID := strings.TrimSpace(reserve.APIKeyID)
	if apiKeyID == "" {
		apiKeyID = safeUsageAPIKeyID(rec.APIKey)
	}
	model := strings.TrimSpace(reserve.Model)
	if model == "" {
		model = rec.Model
	}
	observation := g.ensureHealthObservationLocked(account, model, apiKeyID)
	if at.After(observation.LastRequestAt) {
		observation.LastRequestAt = at
	}
	bucket := g.ensureHealthBucketLocked(observation, at)
	bucket.Requests++
	if rec.Failed {
		bucket.Failures++
		observation.LastFailureAt = at
		class := classifyHealthFailure(rec)
		switch class {
		case "overload":
			bucket.OverloadFailures++
			observation.LastOverloadAt = at
		case "rate_limit":
			bucket.RateLimitFailures++
		case "auth":
			bucket.AuthFailures++
		case "client":
			bucket.ClientFailures++
		case "context":
			bucket.ContextFailures++
		default:
			bucket.OtherFailures++
		}
	} else {
		bucket.Successes++
	}
	score := g.scoreUsage(rec)
	bucket.Tokens += score
	if rec.Latency > 0 {
		bucket.LatencyMillis += rec.Latency.Milliseconds()
		bucket.LatencySamples++
	}
	if rec.TTFT > 0 {
		bucket.TTFTMillis += rec.TTFT.Milliseconds()
		bucket.TTFTSamples++
	}
	if observation.CurrentInflight > bucket.MaxInflight {
		bucket.MaxInflight = observation.CurrentInflight
	}
	g.pruneHealthObservationLocked(observation, at)
}

func (g *quotaGuard) ensureHealthBucketLocked(observation *modelHealthObservation, at time.Time) *healthObservationBucket {
	start := healthBucketStart(at)
	if n := len(observation.Buckets); n > 0 && observation.Buckets[n-1].At.Equal(start) {
		return &observation.Buckets[n-1]
	}
	observation.Buckets = append(observation.Buckets, healthObservationBucket{At: start})
	return &observation.Buckets[len(observation.Buckets)-1]
}

func (g *quotaGuard) pruneHealthObservationLocked(observation *modelHealthObservation, now time.Time) {
	cutoff := now.UTC().Add(-healthObservationRetention)
	kept := observation.Buckets[:0]
	for _, bucket := range observation.Buckets {
		if !bucket.At.Before(cutoff) {
			kept = append(kept, bucket)
		}
	}
	observation.Buckets = kept
}

func classifyHealthFailure(rec pluginapi.UsageRecord) string {
	message := strings.ToLower(strings.TrimSpace(rec.Failure.Body))
	if strings.Contains(message, "server_is_overloaded") || strings.Contains(message, "selected model is at capacity") || strings.Contains(message, "currently overloaded") {
		return "overload"
	}
	if rec.Failure.StatusCode == 429 || strings.Contains(message, "rate limit") {
		return "rate_limit"
	}
	if rec.Failure.StatusCode == 401 || rec.Failure.StatusCode == 403 || strings.Contains(message, "unauthorized") || strings.Contains(message, "cloudflare") {
		return "auth"
	}
	if strings.Contains(message, "context canceled") || strings.Contains(message, "context cancelled") {
		return "client"
	}
	if strings.Contains(message, "context_too_large") || strings.Contains(message, "context window") {
		return "context"
	}
	return "other"
}

func (g *quotaGuard) healthObservationSnapshotLocked(account *accountState, now time.Time) []modelHealthObservation {
	if account == nil {
		return nil
	}
	out := make([]modelHealthObservation, 0, len(account.HealthObservations))
	for _, observation := range account.HealthObservations {
		if observation == nil {
			continue
		}
		g.pruneHealthObservationLocked(observation, now)
		copyObservation := *observation
		copyObservation.Buckets = append([]healthObservationBucket(nil), observation.Buckets...)
		out = append(out, copyObservation)
	}
	return out
}

func healthObservationTotals(observation modelHealthObservation, since time.Time) healthObservationBucket {
	total := healthObservationBucket{}
	for _, bucket := range observation.Buckets {
		if bucket.At.Before(since.UTC()) {
			continue
		}
		total.Requests += bucket.Requests
		total.Successes += bucket.Successes
		total.Failures += bucket.Failures
		total.OverloadFailures += bucket.OverloadFailures
		total.RateLimitFailures += bucket.RateLimitFailures
		total.AuthFailures += bucket.AuthFailures
		total.ClientFailures += bucket.ClientFailures
		total.ContextFailures += bucket.ContextFailures
		total.OtherFailures += bucket.OtherFailures
		total.Tokens += bucket.Tokens
		total.LatencyMillis += bucket.LatencyMillis
		total.LatencySamples += bucket.LatencySamples
		total.TTFTMillis += bucket.TTFTMillis
		total.TTFTSamples += bucket.TTFTSamples
		if bucket.MaxInflight > total.MaxInflight {
			total.MaxInflight = bucket.MaxInflight
		}
	}
	return total
}

func healthObservationJSON(observation modelHealthObservation) string {
	data, errMarshal := json.Marshal(observation)
	if errMarshal != nil {
		return fmt.Sprintf("%s: unavailable", observation.Model)
	}
	return string(data)
}
