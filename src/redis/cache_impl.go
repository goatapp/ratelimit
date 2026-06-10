package redis

import (
	"context"
	"io"
	"math/rand"

	"github.com/coocood/freecache"

	"github.com/goatapp/ratelimit/src/limiter"
	"github.com/goatapp/ratelimit/src/server"
	"github.com/goatapp/ratelimit/src/settings"
	"github.com/goatapp/ratelimit/src/stats"
	"github.com/goatapp/ratelimit/src/utils"
)

func NewRateLimiterCacheImplFromSettings(ctx context.Context, s settings.Settings, localCache *freecache.Cache, srv server.Server, timeSource utils.TimeSource, jitterRand *rand.Rand, expirationJitterMaxSeconds int64, statsManager stats.Manager) (limiter.RateLimitCache, io.Closer) {
	closer := &utils.MultiCloser{}
	var perSecondPool Client
	if s.RedisPerSecond {
		perSecondPool = newClientImpl(ctx, srv.Scope().Scope("redis_per_second_pool"), s.RedisPerSecondTls, s.RedisPerSecondAuth, s.RedisPerSecondSocketType,
			s.RedisPerSecondType, s.RedisPerSecondUrl, s.RedisPerSecondPoolSize, s.RedisPerSecondPipelineWindow, s.RedisPerSecondPipelineLimit, s.RedisTlsConfig, s.RedisHealthCheckActiveConnection, srv, s.RedisPerSecondTimeout,
			s.RedisPerSecondPoolOnEmptyBehavior, s.RedisPerSecondSentinelAuth,
			s.RedisStartupInitialInterval, s.RedisStartupMaxInterval, s.RedisStartupMaxElapsedTime,
			s.RedisPerSecondClusterPipelineParallelism)
		closer.Closers = append(closer.Closers, perSecondPool)
	}

	otherPool := newClientImpl(ctx, srv.Scope().Scope("redis_pool"), s.RedisTls, s.RedisAuth, s.RedisSocketType, s.RedisType, s.RedisUrl, s.RedisPoolSize,
		s.RedisPipelineWindow, s.RedisPipelineLimit, s.RedisTlsConfig, s.RedisHealthCheckActiveConnection, srv, s.RedisTimeout,
		s.RedisPoolOnEmptyBehavior, s.RedisSentinelAuth,
		s.RedisStartupInitialInterval, s.RedisStartupMaxInterval, s.RedisStartupMaxElapsedTime,
		s.RedisClusterPipelineParallelism)
	closer.Closers = append(closer.Closers, otherPool)

	return NewFixedRateLimitCacheImpl(
		otherPool,
		perSecondPool,
		timeSource,
		jitterRand,
		expirationJitterMaxSeconds,
		localCache,
		s.NearLimitRatio,
		s.CacheKeyPrefix,
		statsManager,
		s.StopCacheKeyIncrementWhenOverlimit,
	), closer
}
