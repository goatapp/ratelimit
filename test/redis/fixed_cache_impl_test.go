package redis_test

import (
	"context"
	"math/rand"
	"testing"

	"github.com/goatapp/ratelimit/test/mocks/stats"

	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	gostats "github.com/lyft/gostats"

	"github.com/goatapp/ratelimit/src/config"
	"github.com/goatapp/ratelimit/src/redis"
	"github.com/goatapp/ratelimit/src/trace"
	"github.com/goatapp/ratelimit/src/utils"

	"github.com/stretchr/testify/assert"

	"github.com/goatapp/ratelimit/test/common"
	mock_utils "github.com/goatapp/ratelimit/test/mocks/utils"

	"github.com/golang/mock/gomock"
)

var testSpanExporter = trace.GetTestSpanExporter()

func TestRedisTokenBucketBasic(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	statsStore := gostats.NewStore(gostats.NewNullSink(), false)
	sm := stats.NewMockStatManager(statsStore)
	timeSource := mock_utils.NewMockTimeSource(controller)

	redisSrv := mustNewRedisServer()
	defer redisSrv.Close()

	client := redis.NewClientImpl(context.Background(), statsStore, false, "", "tcp", "single", redisSrv.Addr(), 1, 0, 0, nil, false, nil, 0, "", "", 0, 0, 0)
	cache := redis.NewFixedRateLimitCacheImpl(client, nil, timeSource, rand.New(rand.NewSource(1)), 0, nil, 0.8, "", sm, false)

	// Use a realistic timestamp (June 2026) so PXAT doesn't expire immediately in miniredis
	timeSource.EXPECT().UnixNow().Return(int64(1798825388)).AnyTimes()

	request := common.NewRateLimitRequest("basic_domain", [][][2]string{{{"key", "value"}}}, 1)
	limits := []*config.RateLimit{config.NewRateLimit(10, pb.RateLimitResponse_RateLimit_SECOND, sm.NewStats("basic_key_value"), false, false, false, "", nil, false)}

	response := cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)
	assert.Equal(t, limits[0].Limit, response[0].CurrentLimit)
	assert.Equal(t, uint64(1), limits[0].Stats.TotalHits.Value())
	assert.Equal(t, uint64(1), limits[0].Stats.WithinLimit.Value())
}

func TestRedisTokenBucketOverLimit(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	statsStore := gostats.NewStore(gostats.NewNullSink(), false)
	sm := stats.NewMockStatManager(statsStore)
	timeSource := mock_utils.NewMockTimeSource(controller)

	redisSrv := mustNewRedisServer()
	defer redisSrv.Close()

	client := redis.NewClientImpl(context.Background(), statsStore, false, "", "tcp", "single", redisSrv.Addr(), 1, 0, 0, nil, false, nil, 0, "", "", 0, 0, 0)
	cache := redis.NewFixedRateLimitCacheImpl(client, nil, timeSource, rand.New(rand.NewSource(1)), 0, nil, 0.8, "", sm, false)

	timeSource.EXPECT().UnixNow().Return(int64(1798825388)).AnyTimes()

	request := common.NewRateLimitRequest("overlimit_domain", [][][2]string{{{"key", "value"}}}, 1)
	limits := []*config.RateLimit{config.NewRateLimit(2, pb.RateLimitResponse_RateLimit_SECOND, sm.NewStats("overlimit_key_value"), false, false, false, "", nil, false)}

	// First two requests should pass
	response := cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)

	response = cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)

	// Third request should be over limit
	response = cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OVER_LIMIT, response[0].Code)
}

func TestRedisTokenBucketNilLimit(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	statsStore := gostats.NewStore(gostats.NewNullSink(), false)
	sm := stats.NewMockStatManager(statsStore)
	timeSource := mock_utils.NewMockTimeSource(controller)

	redisSrv := mustNewRedisServer()
	defer redisSrv.Close()

	client := redis.NewClientImpl(context.Background(), statsStore, false, "", "tcp", "single", redisSrv.Addr(), 1, 0, 0, nil, false, nil, 0, "", "", 0, 0, 0)
	cache := redis.NewFixedRateLimitCacheImpl(client, nil, timeSource, rand.New(rand.NewSource(1)), 0, nil, 0.8, "", sm, false)

	timeSource.EXPECT().UnixNow().Return(int64(1798825388)).AnyTimes()

	request := common.NewRateLimitRequest("nil_domain", [][][2]string{{{"key", "value"}}}, 1)
	limits := []*config.RateLimit{nil}

	response := cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)
	assert.Nil(t, response[0].CurrentLimit)
}

func TestRedisTokenBucketDurationReset(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	statsStore := gostats.NewStore(gostats.NewNullSink(), false)
	sm := stats.NewMockStatManager(statsStore)
	timeSource := mock_utils.NewMockTimeSource(controller)

	redisSrv := mustNewRedisServer()
	defer redisSrv.Close()

	client := redis.NewClientImpl(context.Background(), statsStore, false, "", "tcp", "single", redisSrv.Addr(), 1, 0, 0, nil, false, nil, 0, "", "", 0, 0, 0)
	cache := redis.NewFixedRateLimitCacheImpl(client, nil, timeSource, rand.New(rand.NewSource(1)), 0, nil, 0.8, "", sm, false)

	timeSource.EXPECT().UnixNow().Return(int64(1798825388)).AnyTimes()

	request := common.NewRateLimitRequest("reset_domain", [][][2]string{{{"key", "value"}}}, 1)
	limits := []*config.RateLimit{config.NewRateLimit(10, pb.RateLimitResponse_RateLimit_SECOND, sm.NewStats("reset_key_value"), false, false, false, "", nil, false)}

	response := cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)
	assert.Equal(t, utils.CalculateReset(&limits[0].Limit.Unit, timeSource), response[0].DurationUntilReset)
}

func TestRedisTokenBucketShadowMode(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	statsStore := gostats.NewStore(gostats.NewNullSink(), false)
	sm := stats.NewMockStatManager(statsStore)
	timeSource := mock_utils.NewMockTimeSource(controller)

	redisSrv := mustNewRedisServer()
	defer redisSrv.Close()

	client := redis.NewClientImpl(context.Background(), statsStore, false, "", "tcp", "single", redisSrv.Addr(), 1, 0, 0, nil, false, nil, 0, "", "", 0, 0, 0)
	cache := redis.NewFixedRateLimitCacheImpl(client, nil, timeSource, rand.New(rand.NewSource(1)), 0, nil, 0.8, "", sm, false)

	timeSource.EXPECT().UnixNow().Return(int64(1798825388)).AnyTimes()

	request := common.NewRateLimitRequest("shadow_domain", [][][2]string{{{"key", "value"}}}, 1)
	limits := []*config.RateLimit{config.NewRateLimit(1, pb.RateLimitResponse_RateLimit_SECOND, sm.NewStats("shadow_key_value"), false, true, false, "", nil, false)}

	// First request takes the only token
	response := cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)

	// Second request would be over limit, but shadow mode returns OK
	response = cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)
}

func TestRedisTokenBucketLimitRemaining(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	statsStore := gostats.NewStore(gostats.NewNullSink(), false)
	sm := stats.NewMockStatManager(statsStore)
	timeSource := mock_utils.NewMockTimeSource(controller)

	redisSrv := mustNewRedisServer()
	defer redisSrv.Close()

	client := redis.NewClientImpl(context.Background(), statsStore, false, "", "tcp", "single", redisSrv.Addr(), 1, 0, 0, nil, false, nil, 0, "", "", 0, 0, 0)
	cache := redis.NewFixedRateLimitCacheImpl(client, nil, timeSource, rand.New(rand.NewSource(1)), 0, nil, 0.8, "", sm, false)

	timeSource.EXPECT().UnixNow().Return(int64(1798825388)).AnyTimes()

	request := common.NewRateLimitRequest("remaining_domain", [][][2]string{{{"key", "value"}}}, 1)
	limits := []*config.RateLimit{config.NewRateLimit(5, pb.RateLimitResponse_RateLimit_SECOND, sm.NewStats("remaining_key_value"), false, false, false, "", nil, false)}

	// 5 tokens, consume 1 at a time, check LimitRemaining decreases
	response := cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)
	assert.Equal(t, uint32(4), response[0].LimitRemaining)

	response = cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)
	assert.Equal(t, uint32(3), response[0].LimitRemaining)

	response = cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)
	assert.Equal(t, uint32(2), response[0].LimitRemaining)

	response = cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)
	assert.Equal(t, uint32(1), response[0].LimitRemaining)

	response = cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)
	assert.Equal(t, uint32(0), response[0].LimitRemaining)

	// 6th request should be over limit
	response = cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OVER_LIMIT, response[0].Code)
	assert.Equal(t, uint32(0), response[0].LimitRemaining)
}

func TestRedisTokenBucketStopIncrementAllDescriptors(t *testing.T) {
	controller := gomock.NewController(t)
	defer controller.Finish()

	statsStore := gostats.NewStore(gostats.NewNullSink(), false)
	sm := stats.NewMockStatManager(statsStore)
	timeSource := mock_utils.NewMockTimeSource(controller)

	redisSrv := mustNewRedisServer()
	defer redisSrv.Close()

	client := redis.NewClientImpl(context.Background(), statsStore, false, "", "tcp", "single", redisSrv.Addr(), 1, 0, 0, nil, false, nil, 0, "", "", 0, 0, 0)
	cache := redis.NewFixedRateLimitCacheImpl(client, nil, timeSource, rand.New(rand.NewSource(1)), 0, nil, 0.8, "", sm, true)

	timeSource.EXPECT().UnixNow().Return(int64(1798825388)).AnyTimes()

	// Two descriptors: one with limit 2, one with limit 10
	// When key1 goes over limit, ALL descriptors stop consuming tokens
	request := common.NewRateLimitRequest("multi_domain", [][][2]string{{{"key1", "val1"}}, {{"key2", "val2"}}}, 1)
	limits := []*config.RateLimit{
		config.NewRateLimit(2, pb.RateLimitResponse_RateLimit_SECOND, sm.NewStats("multi_key1_val1"), false, false, false, "", nil, false),
		config.NewRateLimit(10, pb.RateLimitResponse_RateLimit_SECOND, sm.NewStats("multi_key2_val2"), false, false, false, "", nil, false),
	}

	// First request: both pass
	response := cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)
	assert.Equal(t, pb.RateLimitResponse_OK, response[1].Code)
	assert.Equal(t, uint32(1), response[0].LimitRemaining)
	assert.Equal(t, uint32(9), response[1].LimitRemaining)

	// Second request: both pass (key1 uses its last token)
	response = cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OK, response[0].Code)
	assert.Equal(t, pb.RateLimitResponse_OK, response[1].Code)
	assert.Equal(t, uint32(0), response[0].LimitRemaining)
	assert.Equal(t, uint32(8), response[1].LimitRemaining)

	// Third request: key1 is over limit, key2 stops consuming too (all descriptors freeze)
	response = cache.DoLimit(context.Background(), request, limits)
	assert.Equal(t, pb.RateLimitResponse_OVER_LIMIT, response[0].Code)
	assert.Equal(t, pb.RateLimitResponse_OK, response[1].Code)
	assert.Equal(t, uint32(8), response[1].LimitRemaining)
}
