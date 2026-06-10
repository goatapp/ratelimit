package runner

import (
	"context"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"

	"github.com/coocood/freecache"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	logger "github.com/goatapp/ratelimit/src/log"
	gostats "github.com/lyft/gostats"

	"github.com/goatapp/ratelimit/src/godogstats"
	"github.com/goatapp/ratelimit/src/limiter"
	"github.com/goatapp/ratelimit/src/memcached"
	"github.com/goatapp/ratelimit/src/metrics"
	"github.com/goatapp/ratelimit/src/redis"
	"github.com/goatapp/ratelimit/src/server"
	ratelimit "github.com/goatapp/ratelimit/src/service"
	"github.com/goatapp/ratelimit/src/settings"
	"github.com/goatapp/ratelimit/src/stats"
	"github.com/goatapp/ratelimit/src/stats/prom"
	"github.com/goatapp/ratelimit/src/trace"
	"github.com/goatapp/ratelimit/src/utils"
)

type Runner struct {
	statsManager    stats.Manager
	settings        settings.Settings
	srv             server.Server
	mu              sync.Mutex
	ratelimitCloser io.Closer
	cancel          context.CancelFunc
	done            chan struct{}
}

func NewRunner(s settings.Settings) Runner {
	var store gostats.Store

	switch {
	case s.DisableStats:
		logger.Info(context.Background(), "Stats disabled")
		store = gostats.NewStore(gostats.NewNullSink(), false)
	case s.UseDogStatsd:
		if s.UseStatsd || s.UsePrometheus {
			logger.Fatal(context.Background(), "Error: unable to use more than one stats sink at the same time. Set one of USE_DOG_STATSD, USE_STATSD, USE_PROMETHEUS.")
		}
		sink, err := godogstats.NewSink(
			godogstats.WithStatsdHost(s.StatsdHost),
			godogstats.WithStatsdPort(s.StatsdPort),
			godogstats.WithMogrifierFromEnv(s.UseDogStatsdMogrifiers))
		if err != nil {
			logger.Fatal(context.Background(), fmt.Sprintf("Failed to create dogstatsd sink: %v", err))
		}
		logger.Info(context.Background(), "Stats initialized for dogstatsd")
		store = gostats.NewStore(sink, false)
	case s.UseStatsd:
		if s.UseDogStatsd || s.UsePrometheus {
			logger.Fatal(context.Background(), "Error: unable to use more than one stats sink at the same time. Set one of USE_DOG_STATSD, USE_STATSD, USE_PROMETHEUS.")
		}
		logger.Info(context.Background(), "Stats initialized for statsd")
		store = gostats.NewStore(gostats.NewTCPStatsdSink(gostats.WithStatsdHost(s.StatsdHost), gostats.WithStatsdPort(s.StatsdPort)), false)
	case s.UsePrometheus:
		if s.UseDogStatsd || s.UseStatsd {
			logger.Fatal(context.Background(), "Error: unable to use more than one stats sink at the same time. Set one of USE_DOG_STATSD, USE_STATSD, USE_PROMETHEUS.")
		}
		logger.Info(context.Background(), "Stats initialized for Prometheus")
		store = gostats.NewStore(prom.NewPrometheusSink(prom.WithAddr(s.PrometheusAddr),
			prom.WithPath(s.PrometheusPath), prom.WithMapperYamlPath(s.PrometheusMapperYaml),
			prom.WithResponseTimeAsMilliseconds(s.PrometheusResponseTimeAsMilliseconds)), false)
	default:
		logger.Info(context.Background(), "Stats initialized for stdout")
		store = gostats.NewStore(gostats.NewLoggingSink(), false)
	}

	logger.Info(context.Background(), fmt.Sprintf("Stats flush interval: %s", s.StatsFlushInterval))

	go store.Start(time.NewTicker(s.StatsFlushInterval))

	return Runner{
		statsManager: stats.NewStatManager(store, s),
		settings:     s,
		done:         make(chan struct{}),
	}
}

func (runner *Runner) GetStatsStore() gostats.Store {
	return runner.statsManager.GetStatsStore()
}

func createLimiter(ctx context.Context, srv server.Server, s settings.Settings, localCache *freecache.Cache, statsManager stats.Manager) (limiter.RateLimitCache, io.Closer) {
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
			statsManager), &utils.MultiCloser{} // memcache client can't be closed
	default:
		logger.Fatal(context.Background(), fmt.Sprintf("Invalid setting for BackendType: %s", s.BackendType))
		panic("This line should not be reachable")
	}
}

func (runner *Runner) Run() {
	defer close(runner.done)

	ctx, cancel := context.WithCancel(context.Background())
	runner.mu.Lock()
	runner.cancel = cancel
	runner.mu.Unlock()
	defer cancel()

	// Set up signal handling for graceful shutdown
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	go func() {
		select {
		case sig := <-sigs:
			logger.Info(context.Background(), fmt.Sprintf("Received signal %v, initiating shutdown", sig))
			cancel()
		case <-ctx.Done():
		}
	}()

	s := runner.settings
	if s.TracingEnabled {
		tp := trace.InitProductionTraceProvider(s.TracingExporterProtocol, s.TracingServiceName, s.TracingServiceNamespace, s.TracingServiceInstanceId, s.TracingSamplingRate)
		defer func() {
			shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer shutdownCancel()
			if err := tp.Shutdown(shutdownCtx); err != nil {
				logger.Error(context.Background(), fmt.Sprintf("Error shutting down tracer provider: %v", err))
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

	srv := server.NewServer(s, "ratelimit", runner.statsManager, localCache, settings.GrpcUnaryInterceptor(serverReporter.UnaryServerInterceptor()))
	runner.mu.Lock()
	runner.srv = srv
	runner.mu.Unlock()

	limiter, limiterCloser := createLimiter(ctx, srv, s, localCache, runner.statsManager)
	runner.ratelimitCloser = limiterCloser
	defer func() {
		if err := limiterCloser.Close(); err != nil {
			logger.Error(context.Background(), fmt.Sprintf("Error closing rate limiter resources: %v", err))
		}
	}()

	service := ratelimit.NewService(
		limiter,
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
			if current, _, _ := service.GetCurrentConfig(); current != nil {
				io.WriteString(writer, current.Dump())
			}
		})

	srv.AddJsonHandler(service)

	// Ratelimit is compatible with the below proto definition
	// data-plane-api v3 rls.proto: https://github.com/envoyproxy/data-plane-api/blob/master/envoy/service/ratelimit/v3/rls.proto
	// v2 proto is no longer supported
	pb.RegisterRateLimitServiceServer(srv.GrpcServer(), service)

	srv.Start(ctx)
}

func (runner *Runner) Stop() {
	runner.mu.Lock()
	cancel := runner.cancel
	runner.mu.Unlock()
	if cancel != nil {
		cancel()
	}
	<-runner.done
}
