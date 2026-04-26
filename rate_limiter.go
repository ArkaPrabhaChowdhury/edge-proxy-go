package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/redis/go-redis/v9"
)

type DecisionAction string

const (
	ActionAllow    DecisionAction = "allow"
	ActionThrottle DecisionAction = "throttle"
	ActionBlock    DecisionAction = "block"
)

type RateLimitDecision struct {
	Action        DecisionAction `json:"action"`
	User          string         `json:"user"`
	Identifier    string         `json:"identifier"`
	Reason        string         `json:"reason"`
	RetryAfter    int            `json:"retry_after_seconds"`
	ThrottleDelay int            `json:"throttle_delay_ms"`
	ObservedRPS   float64        `json:"observed_rps"`
	AverageRPS    float64        `json:"average_rps"`
	WindowCount   int            `json:"window_count"`
	AbuseCount    int            `json:"abuse_count"`
}

type InsightEvent struct {
	Time        string  `json:"time"`
	User        string  `json:"user"`
	Identifier  string  `json:"identifier"`
	Action      string  `json:"action"`
	Reason      string  `json:"reason"`
	ObservedRPS float64 `json:"observed_rps"`
	AverageRPS  float64 `json:"average_rps"`
	WindowCount int     `json:"window_count"`
	AbuseCount  int     `json:"abuse_count"`
	RetryAfter  int     `json:"retry_after_seconds"`
	ThrottleMs  int     `json:"throttle_delay_ms"`
}

type UserTrafficInsight struct {
	User       string `json:"user"`
	Identifier string `json:"identifier"`
	AbuseCount int    `json:"abuse_count"`
}

type rateLimiter struct {
	client *redis.Client
	cfg    RateLimitConfig
}

func newRateLimiter(cfg RateLimitConfig) *rateLimiter {
	return &rateLimiter{
		client: redis.NewClient(&redis.Options{
			Addr:     cfg.Redis.Addr,
			Password: cfg.Redis.Password,
			DB:       cfg.Redis.DB,
		}),
		cfg: cfg,
	}
}

func (r *rateLimiter) ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

func (r *rateLimiter) Evaluate(ctx context.Context, user, identifier string) (RateLimitDecision, error) {
	now := time.Now()
	decision := RateLimitDecision{
		Action:     ActionAllow,
		User:       user,
		Identifier: identifier,
	}

	blockKey := fmt.Sprintf("block:%s", user)
	if ttl, err := r.client.TTL(ctx, blockKey).Result(); err == nil && ttl > 0 {
		decision.Action = ActionBlock
		decision.Reason = "cooldown_active"
		decision.RetryAfter = int(math.Ceil(ttl.Seconds()))
		r.recordEvent(ctx, decision)
		return decision, nil
	}

	windowKey := fmt.Sprintf("rate:%s", user)
	burstKey := fmt.Sprintf("burst:%s", user)
	statsKey := fmt.Sprintf("stats:%s", user)
	abuseKey := fmt.Sprintf("abuse:%s", user)
	nowUnix := now.Unix()
	windowStart := nowUnix - int64(r.cfg.SlidingWindowSeconds)
	member := fmt.Sprintf("%d", now.UnixNano())

	pipe := r.client.TxPipeline()
	pipe.ZRemRangeByScore(ctx, windowKey, "0", strconv.FormatInt(windowStart, 10))
	pipe.ZAdd(ctx, windowKey, redis.Z{Score: float64(nowUnix), Member: member})
	countCmd := pipe.ZCard(ctx, windowKey)
	pipe.Expire(ctx, windowKey, time.Duration(r.cfg.EventRetentionSeconds)*time.Second)
	burstStateCmd := pipe.HMGet(ctx, burstKey, "tokens", "last_refill")
	avgCmd := pipe.HGet(ctx, statsKey, "avg_rps")
	abuseCmd := pipe.Get(ctx, abuseKey)
	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return decision, err
	}

	windowCount := int(countCmd.Val())
	observedRPS := float64(windowCount) / float64(r.cfg.SlidingWindowSeconds)
	decision.WindowCount = windowCount
	decision.ObservedRPS = observedRPS

	tokens := r.cfg.BurstCapacity
	lastRefill := float64(nowUnix)
	if values := burstStateCmd.Val(); len(values) == 2 {
		if values[0] != nil {
			tokens = parseFloat(values[0], tokens)
		}
		if values[1] != nil {
			lastRefill = parseFloat(values[1], lastRefill)
		}
	}

	elapsed := math.Max(0, float64(nowUnix)-lastRefill)
	tokens = math.Min(r.cfg.BurstCapacity, tokens+(elapsed*r.cfg.BaseRequestsPerSecond))

	avgRPS := observedRPS
	if avgCmd.Err() == nil {
		prev := parseFloat(avgCmd.Val(), observedRPS)
		avgRPS = (prev * 0.7) + (observedRPS * 0.3)
	}
	decision.AverageRPS = avgRPS

	if abuseCmd.Err() == nil {
		decision.AbuseCount, _ = strconv.Atoi(abuseCmd.Val())
	}

	spikeThreshold := math.Max(r.cfg.BaseRequestsPerSecond*2, avgRPS*3)
	spiking := observedRPS > spikeThreshold && windowCount > int(math.Ceil(r.cfg.BaseRequestsPerSecond))
	overWindow := observedRPS > r.cfg.BaseRequestsPerSecond
	overBurst := tokens < 1

	switch {
	case spiking || (overWindow && overBurst):
		decision.AbuseCount = r.incrementAbuse(ctx, abuseKey)
		if decision.AbuseCount >= r.cfg.RepeatedAbuseThreshold {
			decision.Action = ActionBlock
			decision.Reason = "repeated_abuse"
			decision.RetryAfter = r.cfg.BlockSeconds
			if err := r.client.Set(ctx, blockKey, "1", time.Duration(r.cfg.BlockSeconds)*time.Second).Err(); err != nil {
				return decision, err
			}
		} else {
			decision.Action = ActionThrottle
			if spiking {
				decision.Reason = "traffic_spike"
			} else {
				decision.Reason = "burst_exceeded"
			}
			decision.ThrottleDelay = r.cfg.SoftThrottleMilliseconds * (decision.AbuseCount + 1)
			decision.RetryAfter = maxInt(1, int(math.Ceil(float64(decision.ThrottleDelay)/1000)))
		}
	case overWindow || overBurst:
		decision.Action = ActionThrottle
		decision.Reason = "adaptive_throttle"
		decision.ThrottleDelay = r.cfg.SoftThrottleMilliseconds
		decision.RetryAfter = maxInt(1, int(math.Ceil(float64(decision.ThrottleDelay)/1000)))
	default:
		tokens--
	}

	pipe = r.client.TxPipeline()
	pipe.HSet(ctx, burstKey, "tokens", tokens, "last_refill", nowUnix)
	pipe.Expire(ctx, burstKey, time.Duration(r.cfg.EventRetentionSeconds)*time.Second)
	pipe.HSet(ctx, statsKey, "avg_rps", avgRPS, "last_seen", nowUnix, "identifier", identifier)
	pipe.Expire(ctx, statsKey, time.Duration(r.cfg.EventRetentionSeconds)*time.Second)
	_, err = pipe.Exec(ctx)
	if err != nil {
		return decision, err
	}

	if decision.Action != ActionAllow {
		r.recordEvent(ctx, decision)
	}
	return decision, nil
}

func (r *rateLimiter) incrementAbuse(ctx context.Context, abuseKey string) int {
	value, err := r.client.Incr(ctx, abuseKey).Result()
	if err != nil {
		return 1
	}
	r.client.Expire(ctx, abuseKey, time.Duration(r.cfg.BlockSeconds*2)*time.Second)
	return int(value)
}

func (r *rateLimiter) recordEvent(ctx context.Context, decision RateLimitDecision) {
	event := InsightEvent{
		Time:        time.Now().Format(time.RFC3339),
		User:        decision.User,
		Identifier:  decision.Identifier,
		Action:      string(decision.Action),
		Reason:      decision.Reason,
		ObservedRPS: roundFloat(decision.ObservedRPS),
		AverageRPS:  roundFloat(decision.AverageRPS),
		WindowCount: decision.WindowCount,
		AbuseCount:  decision.AbuseCount,
		RetryAfter:  decision.RetryAfter,
		ThrottleMs:  decision.ThrottleDelay,
	}
	data, err := json.Marshal(event)
	if err != nil {
		return
	}

	score := float64(time.Now().Unix())
	pipe := r.client.TxPipeline()
	pipe.ZAdd(ctx, "events:rate_limit", redis.Z{Score: score, Member: string(data)})
	if event.Reason == "traffic_spike" {
		pipe.ZAdd(ctx, "events:spikes", redis.Z{Score: score, Member: string(data)})
	}
	pipe.ZAdd(ctx, "events:abusers", redis.Z{Score: float64(event.AbuseCount), Member: fmt.Sprintf("%s|%s", event.User, event.Identifier)})
	pipe.Expire(ctx, "events:rate_limit", time.Duration(r.cfg.EventRetentionSeconds)*time.Second)
	pipe.Expire(ctx, "events:spikes", time.Duration(r.cfg.EventRetentionSeconds)*time.Second)
	pipe.Expire(ctx, "events:abusers", time.Duration(r.cfg.EventRetentionSeconds)*time.Second)
	_, _ = pipe.Exec(ctx)
}

func (r *rateLimiter) TopAbusers(ctx context.Context, limit int64) ([]UserTrafficInsight, error) {
	rows, err := r.client.ZRevRangeWithScores(ctx, "events:abusers", 0, limit-1).Result()
	if err != nil {
		return nil, err
	}
	out := make([]UserTrafficInsight, 0, len(rows))
	for _, row := range rows {
		user, identifier := splitUserIdentifier(fmt.Sprint(row.Member))
		out = append(out, UserTrafficInsight{
			User:       user,
			Identifier: identifier,
			AbuseCount: int(row.Score),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].AbuseCount > out[j].AbuseCount
	})
	return dedupeAbusers(out), nil
}

func (r *rateLimiter) TrafficSpikes(ctx context.Context, limit int64) ([]InsightEvent, error) {
	rows, err := r.client.ZRevRange(ctx, "events:spikes", 0, limit-1).Result()
	if err != nil {
		return nil, err
	}
	return decodeEvents(rows), nil
}

func (r *rateLimiter) RateLimitEvents(ctx context.Context, limit int64) ([]InsightEvent, error) {
	rows, err := r.client.ZRevRange(ctx, "events:rate_limit", 0, limit-1).Result()
	if err != nil {
		return nil, err
	}
	return decodeEvents(rows), nil
}

func decodeEvents(rows []string) []InsightEvent {
	out := make([]InsightEvent, 0, len(rows))
	for _, row := range rows {
		var event InsightEvent
		if err := json.Unmarshal([]byte(row), &event); err == nil {
			out = append(out, event)
		}
	}
	return out
}

func dedupeAbusers(items []UserTrafficInsight) []UserTrafficInsight {
	seen := make(map[string]bool)
	out := make([]UserTrafficInsight, 0, len(items))
	for _, item := range items {
		key := item.User + "|" + item.Identifier
		if seen[key] {
			continue
		}
		seen[key] = true
		out = append(out, item)
	}
	return out
}

func splitUserIdentifier(value string) (string, string) {
	parts := strings.SplitN(value, "|", 2)
	if len(parts) != 2 {
		return value, ""
	}
	return parts[0], parts[1]
}

func parseFloat(value interface{}, fallback float64) float64 {
	switch typed := value.(type) {
	case string:
		if parsed, err := strconv.ParseFloat(typed, 64); err == nil {
			return parsed
		}
	case []byte:
		if parsed, err := strconv.ParseFloat(string(typed), 64); err == nil {
			return parsed
		}
	case int64:
		return float64(typed)
	case float64:
		return typed
	}
	return fallback
}

func roundFloat(value float64) float64 {
	return math.Round(value*100) / 100
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
