package runner

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"sync"
	"time"

	"github.com/goatapp/ratelimit/src/metrics"
	"github.com/goatapp/ratelimit/src/stats"
	"github.com/goatapp/ratelimit/src/trace"

	gostats "github.com/lyft/gostats"

	"github.com/coocood/freecache"

	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"

	"github.com/goatapp/ratelimit/src/limiter"
	logger "github.com/goatapp/ratelimit/src/log"
	"github.com/goatapp/ratelimit/src/memcached"
	"github.com/goatapp/ratelimit/src/redis"
	"github.com/goatapp/ratelimit/src/server"
	ratelimit "github.com/goatapp/ratelimit/src/service"
	"github.com/goatapp/ratelimit/src/settings"
	"github.com/goatapp/ratelimit/src/utils"
)

type Runner struct {
	name         string
	statsManager stats.Manager
	settings     settings.Settings
	srv          server.Server
	mu           sync.Mutex
}

func NewRunner(name string, s settings.Settings) Runner {
	return Runner{
		name:         name,
		statsManager: stats.NewStatManager(gostats.NewDefaultStore(), s),
		settings:     s,
	}
}

func (runner *Runner) GetStatsStore() gostats.Store {
	return runner.statsManager.GetStatsStore()
}

func createLimiter(ctx context.Context, srv server.Server, s settings.Settings, localCache *freecache.Cache, statsManager stats.Manager) limiter.RateLimitCache {
	switch s.BackendType {
	case "redis", "":
		return redis.NewRateLimiterCacheImplFromSettings(
			ctx,
			s,
			localCache,
			srv,
			utils.NewTimeSourceImpl(),
			rand.New(utils.NewLockedSource(time.Now().Unix())),
			s.ExpirationJitterMaxSeconds,
			statsManager,
		)
	case "memcache":
		return memcached.NewRateLimitCacheImplFromSettings(
			s,
			utils.NewTimeSourceImpl(),
			rand.New(utils.NewLockedSource(time.Now().Unix())),
			localCache,
			srv.Scope(),
			statsManager)
	default:
		logger.Fatal(ctx, fmt.Sprintf("Invalid setting for BackendType: %s", s.BackendType))
		panic("This line should not be reachable")
	}
}

func (runner *Runner) Run() {
	s := runner.settings
	if s.TracingEnabled {
		tp := trace.InitProductionTraceProvider(s.TracingExporterProtocol, s.TracingServiceName, s.TracingServiceNamespace, s.TracingServiceInstanceId, s.TracingSamplingRate)
		defer func() {
			if err := tp.Shutdown(context.Background()); err != nil {
				logger.Error(context.Background(), "Error shutting down tracer provider", logger.WithError(err))
			}
		}()
	} else {
		logger.Info(context.Background(), "Tracing disabled")
	}

	var localCache *freecache.Cache
	if s.LocalCacheSizeInBytes != 0 {
		localCache = freecache.NewCache(s.LocalCacheSizeInBytes)
	}

	serverReporter := metrics.NewServerReporter(runner.statsManager.GetStatsStore().ScopeWithTags("ratelimit_server", s.ExtraTags))

	srv := server.NewServer(s, runner.name, runner.statsManager, localCache, settings.GrpcUnaryInterceptor(serverReporter.UnaryServerInterceptor()))
	runner.mu.Lock()
	runner.srv = srv
	runner.mu.Unlock()

	service := ratelimit.NewService(
		createLimiter(context.Background(), srv, s, localCache, runner.statsManager),
		srv.Provider(),
		runner.statsManager,
		srv.HealthChecker(),
		utils.NewTimeSourceImpl(),
		s.GlobalShadowMode,
		s.ForceStartWithoutInitialConfig,
		s.HealthyWithAtLeastOneConfigLoaded,
	)

	srv.AddDebugHttpEndpoint(
		"/rlconfig",
		"print out the currently loaded configuration for debugging",
		func(writer http.ResponseWriter, request *http.Request) {
			if current, _ := service.GetCurrentConfig(); current != nil {
				io.WriteString(writer, current.Dump())
			}
		})

	srv.AddJsonHandler(service)

	// Ratelimit is compatible with the below proto definition
	// data-plane-api v3 rls.proto: https://github.com/envoyproxy/data-plane-api/blob/master/envoy/service/ratelimit/v3/rls.proto
	// v2 proto is no longer supported
	pb.RegisterRateLimitServiceServer(srv.GrpcServer(), service)

	srv.Start()
}

func (runner *Runner) Stop() {
	runner.mu.Lock()
	srv := runner.srv
	runner.mu.Unlock()
	if srv != nil {
		srv.Stop()
	}
}
