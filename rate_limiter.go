package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"sync"
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
	PolicyName    string         `json:"policy_name"`
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
	PolicyName  string  `json:"policy_name"`
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
	mu     sync.Mutex
	local  map[string]localBucket
	events []InsightEvent
}

type localBucket struct {
	windowStart time.Time
	count       int
	blockedTill time.Time
}

func newRateLimiter(cfg RateLimitConfig) *rateLimiter {
	r := &rateLimiter{cfg: cfg, local: make(map[string]localBucket), events: make([]InsightEvent, 0, 100)}
	if strings.TrimSpace(cfg.Redis.Addr) != "" {
		r.client = redis.NewClient(&redis.Options{Addr: cfg.Redis.Addr, Password: cfg.Redis.Password, DB: cfg.Redis.DB})
	}
	return r
}

func (r *rateLimiter) ping(ctx context.Context) error {
	if r.client == nil {
		return nil
	}
	return r.client.Ping(ctx).Err()
}

func (r *rateLimiter) Evaluate(ctx context.Context, user, identifier string, effectiveCfg RateLimitConfig) (RateLimitDecision, error) {
	if r.client == nil {
		return r.evaluateLocal(user, identifier, effectiveCfg), nil
	}
	now := time.Now()
	decision := RateLimitDecision{
		Action:     ActionAllow,
		PolicyName: effectiveCfg.Name,
		User:       user,
		Identifier: identifier,
	}
	scope := effectiveCfg.Name
	if scope == "" {
		scope = "default"
	}

	blockKey := fmt.Sprintf("block:%s:%s", scope, user)
	if ttl, err := r.client.TTL(ctx, blockKey).Result(); err == nil && ttl > 0 {
		decision.Action = ActionBlock
		decision.Reason = "cooldown_active"
		decision.RetryAfter = int(math.Ceil(ttl.Seconds()))
		r.recordEvent(ctx, decision)
		return decision, nil
	}

	windowKey := fmt.Sprintf("rate:%s:%s", scope, user)
	burstKey := fmt.Sprintf("burst:%s:%s", scope, user)
	statsKey := fmt.Sprintf("stats:%s:%s", scope, user)
	abuseKey := fmt.Sprintf("abuse:%s:%s", scope, user)
	nowUnix := now.Unix()
	windowStart := nowUnix - int64(effectiveCfg.SlidingWindowSeconds)
	member := fmt.Sprintf("%d", now.UnixNano())

	pipe := r.client.TxPipeline()
	pipe.ZRemRangeByScore(ctx, windowKey, "0", strconv.FormatInt(windowStart, 10))
	pipe.ZAdd(ctx, windowKey, redis.Z{Score: float64(nowUnix), Member: member})
	countCmd := pipe.ZCard(ctx, windowKey)
	pipe.Expire(ctx, windowKey, time.Duration(effectiveCfg.EventRetentionSeconds)*time.Second)
	burstStateCmd := pipe.HMGet(ctx, burstKey, "tokens", "last_refill")
	avgCmd := pipe.HGet(ctx, statsKey, "avg_rps")
	abuseCmd := pipe.Get(ctx, abuseKey)
	_, err := pipe.Exec(ctx)
	if err != nil && err != redis.Nil {
		return decision, err
	}

	windowCount := int(countCmd.Val())
	observedRPS := float64(windowCount) / float64(effectiveCfg.SlidingWindowSeconds)
	decision.WindowCount = windowCount
	decision.ObservedRPS = observedRPS

	tokens := effectiveCfg.BurstCapacity
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
	tokens = math.Min(effectiveCfg.BurstCapacity, tokens+(elapsed*effectiveCfg.BaseRequestsPerSecond))

	avgRPS := observedRPS
	if avgCmd.Err() == nil {
		prev := parseFloat(avgCmd.Val(), observedRPS)
		avgRPS = (prev * 0.7) + (observedRPS * 0.3)
	}
	decision.AverageRPS = avgRPS

	if abuseCmd.Err() == nil {
		decision.AbuseCount, _ = strconv.Atoi(abuseCmd.Val())
	}

	spikeThreshold := math.Max(effectiveCfg.BaseRequestsPerSecond*2, avgRPS*3)
	spiking := observedRPS > spikeThreshold && windowCount > int(math.Ceil(effectiveCfg.BaseRequestsPerSecond))
	overWindow := observedRPS > effectiveCfg.BaseRequestsPerSecond
	overBurst := tokens < 1

	switch {
	case spiking || (overWindow && overBurst):
		decision.AbuseCount = r.incrementAbuse(ctx, abuseKey, effectiveCfg.BlockSeconds)
		if decision.AbuseCount >= effectiveCfg.RepeatedAbuseThreshold {
			decision.Action = ActionBlock
			decision.Reason = "repeated_abuse"
			decision.RetryAfter = effectiveCfg.BlockSeconds
			if err := r.client.Set(ctx, blockKey, "1", time.Duration(effectiveCfg.BlockSeconds)*time.Second).Err(); err != nil {
				return decision, err
			}
		} else {
			decision.Action = ActionThrottle
			if spiking {
				decision.Reason = "traffic_spike"
			} else {
				decision.Reason = "burst_exceeded"
			}
			decision.ThrottleDelay = effectiveCfg.SoftThrottleMilliseconds * (decision.AbuseCount + 1)
			decision.RetryAfter = maxInt(1, int(math.Ceil(float64(decision.ThrottleDelay)/1000)))
		}
	case overWindow || overBurst:
		decision.Action = ActionThrottle
		decision.Reason = "adaptive_throttle"
		decision.ThrottleDelay = effectiveCfg.SoftThrottleMilliseconds
		decision.RetryAfter = maxInt(1, int(math.Ceil(float64(decision.ThrottleDelay)/1000)))
	default:
		tokens--
	}

	pipe = r.client.TxPipeline()
	pipe.HSet(ctx, burstKey, "tokens", tokens, "last_refill", nowUnix)
	pipe.Expire(ctx, burstKey, time.Duration(effectiveCfg.EventRetentionSeconds)*time.Second)
	pipe.HSet(ctx, statsKey, "avg_rps", avgRPS, "last_seen", nowUnix, "identifier", identifier)
	pipe.Expire(ctx, statsKey, time.Duration(effectiveCfg.EventRetentionSeconds)*time.Second)
	_, err = pipe.Exec(ctx)
	if err != nil {
		return decision, err
	}

	if decision.Action != ActionAllow {
		r.recordEvent(ctx, decision)
	}
	return decision, nil
}

func (r *rateLimiter) evaluateLocal(user, identifier string, effectiveCfg RateLimitConfig) RateLimitDecision {
	now := time.Now()
	decision := RateLimitDecision{Action: ActionAllow, PolicyName: effectiveCfg.Name, User: user, Identifier: identifier}
	key := effectiveCfg.Name + ":" + user
	window := time.Duration(maxInt(effectiveCfg.WindowSeconds, effectiveCfg.SlidingWindowSeconds)) * time.Second
	if window <= 0 {
		window = time.Minute
	}
	limit := effectiveCfg.Requests
	if limit <= 0 {
		limit = maxInt(1, int(math.Ceil(effectiveCfg.BaseRequestsPerSecond*window.Seconds())))
	}
	r.mu.Lock()
	bucket := r.local[key]
	if bucket.windowStart.IsZero() || now.Sub(bucket.windowStart) >= window {
		bucket = localBucket{windowStart: now}
	}
	decision.WindowCount = bucket.count
	if now.Before(bucket.blockedTill) {
		decision.Action = ActionBlock
		decision.Reason = "cooldown_active"
		decision.RetryAfter = maxInt(1, int(math.Ceil(time.Until(bucket.blockedTill).Seconds())))
	} else if bucket.count >= limit {
		decision.Action = ActionBlock
		decision.Reason = "window_exceeded"
		decision.RetryAfter = maxInt(1, int(math.Ceil(time.Until(bucket.windowStart.Add(window)).Seconds())))
		bucket.blockedTill = now.Add(time.Duration(maxInt(1, effectiveCfg.BlockSeconds)) * time.Second)
	} else {
		bucket.count++
	}
	r.local[key] = bucket
	if decision.Action != ActionAllow {
		decision.AbuseCount = bucket.count
		r.events = append(r.events, InsightEvent{Time: now.Format(time.RFC3339), PolicyName: decision.PolicyName, User: user, Identifier: identifier, Action: string(decision.Action), Reason: decision.Reason, WindowCount: bucket.count, AbuseCount: bucket.count, RetryAfter: decision.RetryAfter})
		if len(r.events) > 1000 {
			r.events = r.events[len(r.events)-1000:]
		}
	}
	r.mu.Unlock()
	return decision
}

func (r *rateLimiter) incrementAbuse(ctx context.Context, abuseKey string, blockSeconds int) int {
	value, err := r.client.Incr(ctx, abuseKey).Result()
	if err != nil {
		return 1
	}
	r.client.Expire(ctx, abuseKey, time.Duration(blockSeconds*2)*time.Second)
	return int(value)
}

func (r *rateLimiter) recordEvent(ctx context.Context, decision RateLimitDecision) {
	event := InsightEvent{
		Time:        time.Now().Format(time.RFC3339),
		PolicyName:  decision.PolicyName,
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
	if r.client == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		out := make([]UserTrafficInsight, 0, len(r.local))
		for key, bucket := range r.local {
			parts := strings.SplitN(key, ":", 2)
			user := key
			if len(parts) == 2 {
				user = parts[1]
			}
			out = append(out, UserTrafficInsight{User: user, Identifier: user, AbuseCount: bucket.count})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].AbuseCount > out[j].AbuseCount })
		if int64(len(out)) > limit {
			out = out[:limit]
		}
		return out, nil
	}
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
	if r.client == nil {
		r.mu.Lock()
		defer r.mu.Unlock()
		if int64(len(r.events)) > limit {
			return append([]InsightEvent(nil), r.events[len(r.events)-int(limit):]...), nil
		}
		return append([]InsightEvent(nil), r.events...), nil
	}
	rows, err := r.client.ZRevRange(ctx, "events:spikes", 0, limit-1).Result()
	if err != nil {
		return nil, err
	}
	return decodeEvents(rows), nil
}

func (r *rateLimiter) RateLimitEvents(ctx context.Context, limit int64) ([]InsightEvent, error) {
	if r.client == nil {
		return r.TrafficSpikes(ctx, limit)
	}
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
