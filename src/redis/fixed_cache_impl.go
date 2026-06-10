package redis

import (
	"context"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"github.com/goatapp/ratelimit/src/stats"

	"github.com/coocood/freecache"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/mediocregopher/radix/v4"

	"github.com/goatapp/ratelimit/src/config"
	"github.com/goatapp/ratelimit/src/limiter"
	logger "github.com/goatapp/ratelimit/src/log"
	"github.com/goatapp/ratelimit/src/utils"
)

var script = `
-- ARGV[1] = rate limit key
-- KEYS[1] = token count key
-- KEYS[2] = timestamp key
-- ARGV[1] = tokens per replenish period
-- ARGV[2] = token limit
-- ARGV[3] = replenish period (milliseconds)
-- ARGV[4] = permit count
-- ARGV[5] = current time (unix time milliseconds)
local limit = tonumber(ARGV[2])
local rate = tonumber(ARGV[1])
local period = tonumber(ARGV[3])
local requested = tonumber(ARGV[4])
local now = tonumber(ARGV[5])

local state = redis.call('MGET', KEYS[1], KEYS[2])
local current_tokens = tonumber(state[1]) or limit
local last_refreshed = tonumber(state[2]) or 0

local time_since_last_refreshed = math.max(0, now - last_refreshed)
local periods_since_last_refreshed = math.floor(time_since_last_refreshed / period)

local time_of_last_replenishment = now
if last_refreshed > 0 then
	time_of_last_replenishment = last_refreshed + (periods_since_last_refreshed * period)
end

current_tokens = math.min(limit, current_tokens + (periods_since_last_refreshed * rate))

local allowed = 0
local retry_after = 0
if current_tokens >= requested then
	allowed = 1
	current_tokens = current_tokens - requested

	local periods_until_full = math.ceil(limit / rate)
	local ttl = math.ceil(periods_until_full * period)

	redis.call('SET', KEYS[1], current_tokens, 'PXAT', ttl + now)
	redis.call('SET', KEYS[2], time_of_last_replenishment, 'PXAT', ttl + now)
else
	retry_after = period - (now - time_of_last_replenishment)
end

return { current_tokens, retry_after, allowed }`

var evalScript = radix.NewEvalScript(script)

var tracer = otel.Tracer("redis.fixedCacheImpl")

type fixedRateLimitCacheImpl struct {
	client                             Client
	perSecondClient                    Client
	stopCacheKeyIncrementWhenOverlimit bool
	baseRateLimiter                    *limiter.BaseRateLimiter
}

func pipelineAppendScript(client Client, pipeline *Pipeline, key string, hitsAddend, tokenLimit, tokensPerReplenishPeriod uint32, replenishPeriod, currentTime int64, result *[]int64) {
	keys := []string{fmt.Sprintf("{%s}", key), fmt.Sprintf("{%s}:expires", key)}
	*pipeline = client.PipeScriptAppend(*pipeline, result, evalScript, keys,
		strconv.FormatInt(int64(tokensPerReplenishPeriod), 10),
		strconv.FormatInt(int64(tokenLimit), 10),
		strconv.FormatInt(replenishPeriod, 10),
		strconv.FormatInt(int64(hitsAddend), 10),
		strconv.FormatInt(currentTime, 10))
}

func pipelineAppendtoGet(client Client, pipeline *Pipeline, key string, result *string) {
	*pipeline = client.PipeAppend(*pipeline, result, "GET", key)
}

func (this *fixedRateLimitCacheImpl) DoLimit(
	ctx context.Context,
	request *pb.RateLimitRequest,
	limits []*config.RateLimit,
) []*pb.RateLimitResponse_DescriptorStatus {
	logger.Debug(ctx, "starting cache lookup")

	hitsAddend := max(uint32(1), request.HitsAddend)

	hitsAddends := make([]uint64, len(request.Descriptors))
	for i := range hitsAddends {
		hitsAddends[i] = uint64(hitsAddend)
	}
	cacheKeys := this.baseRateLimiter.GenerateCacheKeys(request, limits, hitsAddends)

	isOverLimitWithLocalCache := make([]bool, len(request.Descriptors))
	results := make([][]int64, len(request.Descriptors))
	for i := range results {
		results[i] = make([]int64, 3)
	}
	currentCount := make([]string, len(request.Descriptors))
	var pipeline, perSecondPipeline, pipelineToGet, perSecondPipelineToGet Pipeline

	hitsAddendForRedis := hitsAddend
	overlimitIndexes := make([]bool, len(request.Descriptors))
	nearlimitIndexes := make([]bool, len(request.Descriptors))
	isCacheKeyOverlimit := false

	if this.stopCacheKeyIncrementWhenOverlimit {
		for i, cacheKey := range cacheKeys {
			if cacheKey.Key == "" {
				continue
			}

			if this.baseRateLimiter.IsOverLimitWithLocalCache(cacheKey.Key) {
				if limits[i].ShadowMode {
					logger.Debug(ctx, fmt.Sprintf("Cache key %s would be rate limited but shadow mode is enabled on this rule", cacheKey.Key))
				} else {
					logger.Debug(ctx, fmt.Sprintf("cache key is over the limit: %s", cacheKey.Key))
				}
				isOverLimitWithLocalCache[i] = true
				hitsAddendForRedis = 0
				overlimitIndexes[i] = true
				isCacheKeyOverlimit = true
				continue
			} else {
				if this.perSecondClient != nil && cacheKey.PerSecond {
					if perSecondPipelineToGet == nil {
						perSecondPipelineToGet = Pipeline{}
					}
					pipelineAppendtoGet(this.perSecondClient, &perSecondPipelineToGet, fmt.Sprintf("{%s}", cacheKey.Key), &currentCount[i])
				} else {
					if pipelineToGet == nil {
						pipelineToGet = Pipeline{}
					}
					pipelineAppendtoGet(this.client, &pipelineToGet, fmt.Sprintf("{%s}", cacheKey.Key), &currentCount[i])
				}
			}
		}

		if len(cacheKeys) > 1 && !isCacheKeyOverlimit {
			if pipelineToGet != nil {
				checkError(this.client.PipeDo(ctx, pipelineToGet))
			}
			if perSecondPipelineToGet != nil {
				checkError(this.perSecondClient.PipeDo(ctx, perSecondPipelineToGet))
			}

			for i, cacheKey := range cacheKeys {
				if cacheKey.Key == "" {
					continue
				}
				// In token bucket mode: empty string means key doesn't exist (bucket is full).
				// A value of "0" means the bucket is actually empty.
				var tokensRemaining uint32
				if currentCount[i] == "" {
					tokensRemaining = limits[i].Limit.RequestsPerUnit
				} else {
					parsed, _ := strconv.ParseUint(currentCount[i], 10, 32)
					tokensRemaining = uint32(parsed)
				}
				allowed := tokensRemaining >= hitsAddend
				limitAfterIncrease := getLimitAfterIncrease(tokensRemaining, limits[i].Limit.RequestsPerUnit, hitsAddend, allowed)
				limitBeforeIncrease := limitAfterIncrease - hitsAddend

				limitInfo := limiter.NewRateLimitInfo(limits[i], uint64(limitBeforeIncrease), uint64(limitAfterIncrease), 0, 0)

				if this.baseRateLimiter.IsOverLimitThresholdReached(limitInfo) {
					hitsAddendForRedis = 0
					nearlimitIndexes[i] = true
				}
			}
		}
	} else {
		for i, cacheKey := range cacheKeys {
			if cacheKey.Key == "" {
				continue
			}

			if this.baseRateLimiter.IsOverLimitWithLocalCache(cacheKey.Key) {
				if limits[i].ShadowMode {
					logger.Debug(ctx, fmt.Sprintf("Cache key %s would be rate limited but shadow mode is enabled on this rule", cacheKey.Key))
				} else {
					logger.Debug(ctx, fmt.Sprintf("cache key is over the limit: %s", cacheKey.Key))
				}
				isOverLimitWithLocalCache[i] = true
				overlimitIndexes[i] = true
				continue
			}
		}
	}

	for i, cacheKey := range cacheKeys {
		if cacheKey.Key == "" || overlimitIndexes[i] {
			continue
		}

		logger.Debug(ctx, fmt.Sprintf("looking up cache key: %s", cacheKey.Key))

		replenishPeriod := time.Duration(utils.UnitToDivider(limits[i].Limit.Unit) * int64(time.Second)).Milliseconds()
		if replenishPeriod == 1000 {
			replenishPeriod = 775
		}

		unixTime := this.baseRateLimiter.TimeSource.UnixNow() * 1000

		if this.perSecondClient != nil && cacheKey.PerSecond {
			if perSecondPipeline == nil {
				perSecondPipeline = Pipeline{}
			}

			pipelineAppendScript(this.perSecondClient, &perSecondPipeline, cacheKey.Key, hitsAddendForRedis, limits[i].Limit.RequestsPerUnit, limits[i].Limit.RequestsPerUnit, replenishPeriod, unixTime, &results[i])
		} else {
			if pipeline == nil {
				pipeline = Pipeline{}
			}

			pipelineAppendScript(this.client, &pipeline, cacheKey.Key, hitsAddendForRedis, limits[i].Limit.RequestsPerUnit, limits[i].Limit.RequestsPerUnit, replenishPeriod, unixTime, &results[i])
		}
	}

	_, span := tracer.Start(
		ctx, "Redis Pipeline Execution",
		trace.WithAttributes(
			attribute.Int("pipeline length", len(pipeline)),
			attribute.Int("perSecondPipeline length", len(perSecondPipeline)),
		),
	)
	defer span.End()

	if pipeline != nil {
		checkError(this.client.PipeDo(ctx, pipeline))
	}
	if perSecondPipeline != nil {
		checkError(this.perSecondClient.PipeDo(ctx, perSecondPipeline))
	}

	responseDescriptorStatuses := make([]*pb.RateLimitResponse_DescriptorStatus,
		len(request.Descriptors))
	for i, cacheKey := range cacheKeys {
		limitAfterIncrease := uint32(0)
		limitBeforeIncrease := uint32(0)
		if limits[i] != nil {
			currentTokens := uint32(results[i][0])
			allowed := results[i][2] != 0

			limitAfterIncrease = getLimitAfterIncrease(currentTokens, limits[i].Limit.RequestsPerUnit, hitsAddendForRedis, allowed)
			limitBeforeIncrease = limitAfterIncrease - hitsAddendForRedis

			logger.Debug(ctx, fmt.Sprintf("pipeline result cache key %s current: %d", cacheKey.Key, limitAfterIncrease), logger.WithValue("redisKey", cacheKey.Key), logger.WithValue("redisCurrentTokens", currentTokens),
				logger.WithValue("redisAllowed", allowed), logger.WithValue("redisRetryAfter", results[i][1]), logger.WithValue("redisLimitAfterIncrease", limitAfterIncrease))
		}

		limitInfo := limiter.NewRateLimitInfo(limits[i], uint64(limitBeforeIncrease), uint64(limitAfterIncrease), 0, 0)

		responseDescriptorStatuses[i] = this.baseRateLimiter.GetResponseDescriptorStatus(cacheKey.Key,
			limitInfo, isOverLimitWithLocalCache[i], uint64(hitsAddend))
	}

	return responseDescriptorStatuses
}

func getLimitAfterIncrease(currentTokens, requestsPerUnit, hitsAddend uint32, allowed bool) uint32 {
	if hitsAddend == 0 {
		if currentTokens == 0 {
			return requestsPerUnit + 1
		}
		return requestsPerUnit - currentTokens
	}

	if currentTokens == 0 {
		limitAfterIncrease := requestsPerUnit
		if !allowed {
			limitAfterIncrease = limitAfterIncrease + hitsAddend
		}
		return limitAfterIncrease
	}

	limitAfterIncrease := hitsAddend + requestsPerUnit - currentTokens
	if allowed {
		limitAfterIncrease = limitAfterIncrease - 1
	}
	return limitAfterIncrease
}

func (this *fixedRateLimitCacheImpl) Flush() {}

func NewFixedRateLimitCacheImpl(client Client, perSecondClient Client, timeSource utils.TimeSource,
	jitterRand *rand.Rand, expirationJitterMaxSeconds int64, localCache *freecache.Cache, nearLimitRatio float32, cacheKeyPrefix string, statsManager stats.Manager,
	stopCacheKeyIncrementWhenOverlimit bool,
) limiter.RateLimitCache {
	return &fixedRateLimitCacheImpl{
		client:                             client,
		perSecondClient:                    perSecondClient,
		stopCacheKeyIncrementWhenOverlimit: stopCacheKeyIncrementWhenOverlimit,
		baseRateLimiter:                    limiter.NewBaseRateLimit(timeSource, jitterRand, expirationJitterMaxSeconds, localCache, nearLimitRatio, cacheKeyPrefix, statsManager),
	}
}
