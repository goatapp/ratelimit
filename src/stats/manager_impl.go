package stats

import (
	"context"
	"fmt"
	"os"

	gostats "github.com/lyft/gostats"

	logger "github.com/goatapp/ratelimit/src/log"
	"github.com/goatapp/ratelimit/src/settings"
	"github.com/goatapp/ratelimit/src/utils"
)

func NewStatManager(store gostats.Store, settings settings.Settings) *ManagerImpl {
	serviceScope := store.ScopeWithTags(GetStatsScope(), settings.ExtraTags).Scope("service")
	return &ManagerImpl{
		store:                store,
		rlStatsScope:         serviceScope.Scope("rate_limit"),
		serviceStatsScope:    serviceScope,
		shouldRateLimitScope: serviceScope.Scope("call.should_rate_limit"),
	}
}

func GetStatsScope() string {
	appName := os.Getenv("GOATENV_APP")

	if appName == "" {
		appName = "ratelimit"
	}

	appEnv := os.Getenv("GOATENV_ENVIRONMENT")

	if appEnv == "" {
		appEnv = "local"
	}

	return fmt.Sprintf("app.%v.%v", appName, appEnv)
}

func (this *ManagerImpl) GetStatsStore() gostats.Store {
	return this.store
}

// Create new rate descriptor stats for a descriptor tuple.
// @param key supplies the fully resolved descriptor tuple.
// @return new stats.
func (this *ManagerImpl) NewStats(key string) RateLimitStats {
	ret := RateLimitStats{}
	logger.Debug(context.Background(), fmt.Sprintf("Creating stats for key: '%s'", key))
	ret.Key = key
	key = utils.SanitizeStatName(key)
	ret.TotalHits = this.rlStatsScope.NewCounter(key + ".total_hits")
	ret.OverLimit = this.rlStatsScope.NewCounter(key + ".over_limit")
	ret.NearLimit = this.rlStatsScope.NewCounter(key + ".near_limit")
	ret.OverLimitWithLocalCache = this.rlStatsScope.NewCounter(key + ".over_limit_with_local_cache")
	ret.WithinLimit = this.rlStatsScope.NewCounter(key + ".within_limit")
	ret.ShadowMode = this.rlStatsScope.NewCounter(key + ".shadow_mode")
	return ret
}

func (this *ManagerImpl) NewShouldRateLimitStats() ShouldRateLimitStats {
	ret := ShouldRateLimitStats{}
	ret.RedisError = this.shouldRateLimitScope.NewCounter("redis_error")
	ret.ServiceError = this.shouldRateLimitScope.NewCounter("service_error")
	return ret
}

func (this *ManagerImpl) NewServiceStats() ServiceStats {
	ret := ServiceStats{}
	ret.ConfigLoadSuccess = this.serviceStatsScope.NewCounter("config_load_success")
	ret.ConfigLoadError = this.serviceStatsScope.NewCounter("config_load_error")
	ret.ShouldRateLimit = this.NewShouldRateLimitStats()
	ret.GlobalShadowMode = this.serviceStatsScope.NewCounter("global_shadow_mode")
	return ret
}

func (this RateLimitStats) GetKey() string {
	return this.Key
}
