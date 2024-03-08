package redis

import (
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
	"golang.org/x/net/context"

	"github.com/goatapp/ratelimit/src/config"
	"github.com/goatapp/ratelimit/src/limiter"
	logger "github.com/goatapp/ratelimit/src/log"
	"github.com/goatapp/ratelimit/src/utils"
)

var script = `
-- ARGV[1] = rate limit key
-- ARGV[2] = timestamp key
-- ARGV[3] = tokens per replenish period
-- ARGV[4] = token limit
-- ARGV[5] = replenish period (milliseconds)
-- ARGV[6] = permit count
-- ARGV[7] = current time (unix time milliseconds)
-- Prepare the input and force the correct data types.
local limit = tonumber(ARGV[4])
local rate = tonumber(ARGV[3])
local period = tonumber(ARGV[5])
local requested = tonumber(ARGV[6])
local now = tonumber(ARGV[7])

-- Load the current state from Redis. We use MGET to save a round-trip.
local state = redis.call('MGET', ARGV[1], ARGV[2])
local current_tokens = tonumber(state[1]) or limit
local last_refreshed = tonumber(state[2]) or 0

-- Calculate the time and replenishment periods elapsed since the last call.
local time_since_last_refreshed = math.max(0, now - last_refreshed)
local periods_since_last_refreshed = math.floor(time_since_last_refreshed / period)

-- We are also able to calculate the time of the last replenishment, which we store and use
-- to calculate the time after which a client may retry if they are rate limited.
local time_of_last_replenishment = now
if last_refreshed > 0 then
	time_of_last_replenishment = last_refreshed + (periods_since_last_refreshed * period)
end

-- Now we have all the info we need to calculate the current tokens based on the elapsed time.
current_tokens = math.min(limit, current_tokens + (periods_since_last_refreshed * rate))

-- If the bucket contains enough tokens for the current request, we remove the tokens.
local allowed = 0
local retry_after = 0
if current_tokens >= requested then
	allowed = 1
	current_tokens = current_tokens - requested

-- In order to remove rate limit keys automatically from the database, we calculate a TTL
-- based on the worst-case scenario for the bucket to fill up again.
-- The worst case is when the bucket is empty and the last replenishment adds less tokens than available.
	local periods_until_full = math.ceil(limit / rate)
	local ttl = math.ceil(periods_until_full * period)

-- We only store the new state in the database if the request was granted.
-- This avoids rounding issues and edge cases which can occur if many requests are rate limited.
	redis.call('SET', ARGV[1], current_tokens, 'PXAT', ttl + now)
	redis.call('SET', ARGV[2], time_of_last_replenishment, 'PXAT', ttl + now)
else
-- Before we return, we can now also calculate when the client may retry again if they are rate limited.
	retry_after = period - (now - time_of_last_replenishment)
end

return { current_tokens, retry_after, allowed }`

var evalScript = radix.NewEvalScript(script)

var tracer = otel.Tracer("redis.fixedCacheImpl")

type fixedRateLimitCacheImpl struct {
	client Client
	// Optional Client for a dedicated cache of per second limits.
	// If this client is nil, then the Cache will use the client for all
	// limits regardless of unit. If this client is not nil, then it
	// is used for limits that have a SECOND unit.
	perSecondClient                    Client
	stopCacheKeyIncrementWhenOverlimit bool
	baseRateLimiter                    *limiter.BaseRateLimiter
}

func pipelineAppendScript(client Client, pipeline *Pipeline, key string, hitsAddend, tokenLimit, tokensPerReplenishPeriod uint32, replenishPeriod, currentTime int64, result *[]int64) {
	*pipeline = client.PipeScriptAppend(*pipeline, result, evalScript,
		key,
		fmt.Sprintf("%s:expires", key),
		strconv.FormatInt(int64(tokensPerReplenishPeriod), 10),
		strconv.FormatInt(int64(tokenLimit), 10),
		strconv.FormatInt(replenishPeriod, 10),
		strconv.FormatInt(int64(hitsAddend), 10),
		strconv.FormatInt(currentTime, 10))
}

func pipelineAppendtoGet(client Client, pipeline *Pipeline, key string, result *uint32) {
	*pipeline = client.PipeAppend(*pipeline, result, "GET", key)
}

func (this *fixedRateLimitCacheImpl) DoLimit(
	ctx context.Context,
	request *pb.RateLimitRequest,
	limits []*config.RateLimit) []*pb.RateLimitResponse_DescriptorStatus {

	logger.Debug(ctx, "starting cache lookup")

	// request.HitsAddend could be 0 (default value) if not specified by the caller in the RateLimit request.
	hitsAddend := utils.Max(1, request.HitsAddend)

	// First build a list of all cache keys that we are actually going to hit.
	cacheKeys := this.baseRateLimiter.GenerateCacheKeys(request, limits, hitsAddend)

	isOverLimitWithLocalCache := make([]bool, len(request.Descriptors))
	results := make([][]int64, len(request.Descriptors))
	for i := range results {
		results[i] = make([]int64, 3)
	}
	currentCount := make([]uint32, len(request.Descriptors))
	var pipeline, perSecondPipeline, pipelineToGet, perSecondPipelineToGet Pipeline

	hitsAddendForRedis := hitsAddend
	overlimitIndexes := make([]bool, len(request.Descriptors))
	nearlimitIndexes := make([]bool, len(request.Descriptors))
	isCacheKeyOverlimit := false

	if this.stopCacheKeyIncrementWhenOverlimit {
		// Check if any of the keys are reaching to the over limit in redis cache.
		for i, cacheKey := range cacheKeys {
			if cacheKey.Key == "" {
				continue
			}

			// Check if key is over the limit in local cache.
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
					pipelineAppendtoGet(this.perSecondClient, &perSecondPipelineToGet, cacheKey.Key, &currentCount[i])
				} else {
					if pipelineToGet == nil {
						pipelineToGet = Pipeline{}
					}
					pipelineAppendtoGet(this.client, &pipelineToGet, cacheKey.Key, &currentCount[i])
				}
			}
		}

		// Only if none of the cache keys exceed the limit, call Redis to check whether the cache keys are becoming overlimited.
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
				// Now fetch the pipeline.
				allowed := currentCount[i] >= hitsAddend
				limitAfterIncrease := getLimitAfterIncrease(currentCount[i], limits[i].Limit.RequestsPerUnit, hitsAddend, allowed)
				limitBeforeIncrease := limitAfterIncrease - hitsAddend

				limitInfo := limiter.NewRateLimitInfo(limits[i], limitBeforeIncrease, limitAfterIncrease, 0, 0)

				if this.baseRateLimiter.IsOverLimitThresholdReached(limitInfo) {
					hitsAddendForRedis = 0
					nearlimitIndexes[i] = true
				}
			}
		}
	} else {
		// Check if any of the keys are reaching to the over limit in redis cache.
		for i, cacheKey := range cacheKeys {
			if cacheKey.Key == "" {
				continue
			}

			// Check if key is over the limit in local cache.
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

	// Now, actually set up the pipeline, skipping empty cache keys.
	for i, cacheKey := range cacheKeys {
		if cacheKey.Key == "" || overlimitIndexes[i] {
			continue
		}

		logger.Debug(ctx, fmt.Sprintf("looking up cache key: %s", cacheKey.Key))

		replenishPeriod := time.Duration(utils.UnitToDivider(limits[i].Limit.Unit) * int64(time.Second)).Milliseconds()
		if replenishPeriod == 1000 { // adjusting the period for RPS since in practice the TTL expires later than expected leading to over-counting
			replenishPeriod = 900
		}

		unixTime := this.baseRateLimiter.TimeSource.UnixNow()

		// Use the perSecondConn if it is not nil and the cacheKey represents a per second Limit.
		if this.perSecondClient != nil && cacheKey.PerSecond {
			if perSecondPipeline == nil {
				perSecondPipeline = Pipeline{}
			}

			hitsAddendToUse := hitsAddendForRedis
			if !nearlimitIndexes[i] {
				hitsAddendToUse = hitsAddend
			}

			pipelineAppendScript(this.perSecondClient, &perSecondPipeline, cacheKey.Key, hitsAddendToUse, limits[i].Limit.RequestsPerUnit, limits[i].Limit.RequestsPerUnit, replenishPeriod, unixTime, &results[i])
		} else {
			if pipeline == nil {
				pipeline = Pipeline{}
			}

			hitsAddendToUse := hitsAddendForRedis
			if !nearlimitIndexes[i] {
				hitsAddendToUse = hitsAddend
			}

			pipelineAppendScript(this.client, &pipeline, cacheKey.Key, hitsAddendToUse, limits[i].Limit.RequestsPerUnit, limits[i].Limit.RequestsPerUnit, replenishPeriod, unixTime, &results[i])
		}
	}

	// Generate trace
	_, span := tracer.Start(ctx, "Redis Pipeline Execution",
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

	// Now fetch the pipeline.
	responseDescriptorStatuses := make([]*pb.RateLimitResponse_DescriptorStatus,
		len(request.Descriptors))
	for i, cacheKey := range cacheKeys {
		limitAfterIncrease := uint32(0)
		limitBeforeIncrease := uint32(0)
		if limits[i] != nil {
			currentTokens := uint32(results[i][0])
			allowed := results[i][2] != 0

			limitAfterIncrease = getLimitAfterIncrease(currentTokens, limits[i].Limit.RequestsPerUnit, hitsAddend, allowed)
			limitBeforeIncrease = limitAfterIncrease - hitsAddend

			logger.Debug(ctx, fmt.Sprintf("pipeline result cache key %s current: %d", cacheKey.Key, limitAfterIncrease), logger.WithValue("redisKey", cacheKey.Key), logger.WithValue("redisCurrentTokens", currentTokens),
				logger.WithValue("redisAllowed", allowed), logger.WithValue("redisRetryAfter", results[i][1]), logger.WithValue("redisLimitAfterIncrease", limitAfterIncrease))
		}

		limitInfo := limiter.NewRateLimitInfo(limits[i], limitBeforeIncrease, limitAfterIncrease, 0, 0)

		responseDescriptorStatuses[i] = this.baseRateLimiter.GetResponseDescriptorStatus(ctx, cacheKey.Key,
			limitInfo, isOverLimitWithLocalCache[i], hitsAddend)

	}

	return responseDescriptorStatuses
}

func getLimitAfterIncrease(currentTokens, requestsPerUnit, hitsAddend uint32, allowed bool) uint32 {
	limitAfterIncrease := uint32(0)

	if currentTokens == 0 {
		limitAfterIncrease = requestsPerUnit
		if !allowed {
			limitAfterIncrease = limitAfterIncrease + hitsAddend
		}
	} else {
		limitAfterIncrease = hitsAddend + requestsPerUnit - currentTokens
		if allowed {
			limitAfterIncrease = limitAfterIncrease - 1
		}
	}

	return limitAfterIncrease
}

// Flush() is a no-op with redis since quota reads and updates happen synchronously.
func (this *fixedRateLimitCacheImpl) Flush() {}

func NewFixedRateLimitCacheImpl(client Client, perSecondClient Client, timeSource utils.TimeSource,
	jitterRand *rand.Rand, expirationJitterMaxSeconds int64, localCache *freecache.Cache, nearLimitRatio float32, cacheKeyPrefix string, statsManager stats.Manager,
	stopCacheKeyIncrementWhenOverlimit bool) limiter.RateLimitCache {
	return &fixedRateLimitCacheImpl{
		client:                             client,
		perSecondClient:                    perSecondClient,
		stopCacheKeyIncrementWhenOverlimit: stopCacheKeyIncrementWhenOverlimit,
		baseRateLimiter:                    limiter.NewBaseRateLimit(timeSource, jitterRand, expirationJitterMaxSeconds, localCache, nearLimitRatio, cacheKeyPrefix, statsManager),
	}
}
