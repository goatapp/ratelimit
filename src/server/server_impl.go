package server

import (
	"context"
	"expvar"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"syscall"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/goatapp/ratelimit/src/provider"
	"github.com/goatapp/ratelimit/src/stats"

	"github.com/coocood/freecache"
	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/gorilla/mux"
	reuseport "github.com/kavu/go_reuseport"
	"github.com/lyft/goruntime/loader"
	gostats "github.com/lyft/gostats"
	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"

	"github.com/goatapp/ratelimit/src/limiter"
	logger "github.com/goatapp/ratelimit/src/log"
	"github.com/goatapp/ratelimit/src/settings"

	"go.opentelemetry.io/contrib/instrumentation/google.golang.org/grpc/otelgrpc"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

var tracer = otel.Tracer("ratelimit server")

type serverDebugListener struct {
	endpoints map[string]string
	debugMux  *http.ServeMux
	listener  net.Listener
}

type server struct {
	httpAddress   string
	grpcAddress   string
	debugAddress  string
	router        *mux.Router
	grpcServer    *grpc.Server
	store         gostats.Store
	scope         gostats.Scope
	provider      provider.RateLimitConfigProvider
	runtime       loader.IFace
	debugListener serverDebugListener
	httpServer    *http.Server
	listenerMu    sync.Mutex
	health        *HealthChecker
}

func (server *server) AddDebugHttpEndpoint(path string, help string, handler http.HandlerFunc) {
	server.listenerMu.Lock()
	defer server.listenerMu.Unlock()
	server.debugListener.debugMux.HandleFunc(path, handler)
	server.debugListener.endpoints[path] = help
}

// create an http/1 handler at the /json endpoint which allows this ratelimit service to work with
// clients that cannot use the gRPC interface (e.g. lua)
// example usage from cURL with domain "dummy" and descriptor "perday":
// echo '{"domain": "dummy", "descriptors": [{"entries": [{"key": "perday"}]}]}' | curl -vvvXPOST --data @/dev/stdin localhost:8080/json
func NewJsonHandler(svc pb.RateLimitServiceServer) func(http.ResponseWriter, *http.Request) {
	return func(writer http.ResponseWriter, request *http.Request) {
		var req pb.RateLimitRequest

		ctx := context.Background()

		body, err := io.ReadAll(request.Body)
		if err != nil {
			logger.Error(ctx, "", logger.WithError(err))
			writeHttpStatus(writer, http.StatusBadRequest)
			return
		}

		if err := protojson.Unmarshal(body, &req); err != nil {
			logger.Error(ctx, "", logger.WithError(err))
			writeHttpStatus(writer, http.StatusBadRequest)
			return
		}

		resp, err := svc.ShouldRateLimit(ctx, &req)
		if err != nil {
			logger.Error(ctx, "", logger.WithError(err))
			writeHttpStatus(writer, http.StatusBadRequest)
			return
		}

		// Generate trace
		_, span := tracer.Start(ctx, "NewJsonHandler Remaining Execution",
			trace.WithAttributes(
				attribute.String("response", resp.String()),
			),
		)
		defer span.End()

		logger.Debug(ctx, fmt.Sprintf("resp:%s", resp))
		if resp == nil {
			logger.Error(ctx, "nil response")
			writeHttpStatus(writer, http.StatusInternalServerError)
			return
		}

		jsonResp, err := protojson.Marshal(resp)
		if err != nil {
			logger.Error(ctx, "error marshaling proto3 to json", logger.WithError(err))
			writeHttpStatus(writer, http.StatusInternalServerError)
			return
		}

		writer.Header().Set("Content-Type", "application/json")
		if resp == nil || resp.OverallCode == pb.RateLimitResponse_UNKNOWN {
			writer.WriteHeader(http.StatusInternalServerError)
		} else if resp.OverallCode == pb.RateLimitResponse_OVER_LIMIT {
			writer.WriteHeader(http.StatusTooManyRequests)
		}
		writer.Write(jsonResp)
	}
}

func writeHttpStatus(writer http.ResponseWriter, code int) {
	http.Error(writer, http.StatusText(code), code)
}

func getProviderImpl(s settings.Settings, statsManager stats.Manager, rootStore gostats.Store) provider.RateLimitConfigProvider {
	switch s.ConfigType {
	case "FILE":
		return provider.NewFileProvider(s, statsManager, rootStore)
	case "GRPC_XDS_SOTW":
		return provider.NewXdsGrpcSotwProvider(s, statsManager)
	default:
		logger.Fatal(context.Background(), fmt.Sprintf("Invalid setting for ConfigType: %s", s.ConfigType))
		panic("This line should not be reachable")
	}
}

func (server *server) AddJsonHandler(svc pb.RateLimitServiceServer) {
	server.router.HandleFunc("/json", NewJsonHandler(svc))
}

func (server *server) GrpcServer() *grpc.Server {
	return server.grpcServer
}

func (server *server) Start() {
	server.startGrpc()

	server.handleGracefulShutdown()
}

func (server *server) startGrpc() {
	logger.Warn(context.Background(), fmt.Sprintf("Listening for gRPC on '%s'", server.grpcAddress))
	lis, err := reuseport.Listen("tcp", server.grpcAddress)
	if err != nil {
		logger.Fatal(context.Background(), fmt.Sprintf("Failed to listen for gRPC: %v", err))
	}
	server.grpcServer.Serve(lis)
}

func (server *server) Scope() gostats.Scope {
	return server.scope
}

func (server *server) Provider() provider.RateLimitConfigProvider {
	return server.provider
}

func NewServer(s settings.Settings, name string, statsManager stats.Manager, localCache *freecache.Cache, opts ...settings.Option) Server {
	return newServer(s, name, statsManager, localCache, opts...)
}

func newServer(s settings.Settings, name string, statsManager stats.Manager, localCache *freecache.Cache, opts ...settings.Option) *server {
	for _, opt := range opts {
		opt(&s)
	}

	ret := new(server)

	keepaliveOpt := grpc.KeepaliveParams(keepalive.ServerParameters{
		MaxConnectionAge:      s.GrpcMaxConnectionAge,
		MaxConnectionAgeGrace: s.GrpcMaxConnectionAgeGrace,
	})
	grpcOptions := []grpc.ServerOption{
		keepaliveOpt,
		grpc.ChainUnaryInterceptor(
			s.GrpcUnaryInterceptor, // chain otel interceptor after the input interceptor
			otelgrpc.UnaryServerInterceptor(),
		),
		grpc.StreamInterceptor(otelgrpc.StreamServerInterceptor()),
	}
	if s.GrpcServerUseTLS {
		grpcServerTlsConfig := s.GrpcServerTlsConfig
		// Verify client SAN if provided
		if s.GrpcClientTlsSAN != "" {
			grpcServerTlsConfig.VerifyPeerCertificate = verifyClient(grpcServerTlsConfig.ClientCAs, s.GrpcClientTlsSAN)
		}
		grpcOptions = append(grpcOptions, grpc.Creds(credentials.NewTLS(grpcServerTlsConfig)))
	}
	ret.grpcServer = grpc.NewServer(grpcOptions...)

	// setup listen addresses
	ret.httpAddress = net.JoinHostPort(s.Host, strconv.Itoa(s.Port))
	ret.grpcAddress = net.JoinHostPort(s.GrpcHost, strconv.Itoa(s.GrpcPort))
	ret.debugAddress = net.JoinHostPort(s.DebugHost, strconv.Itoa(s.DebugPort))

	// setup stats
	ret.store = statsManager.GetStatsStore()
	ret.scope = ret.store.ScopeWithTags(stats.GetStatsScope(), s.ExtraTags)
	if localCache != nil {
		ret.store.AddStatGenerator(limiter.NewLocalCacheStats(localCache, ret.scope.Scope("localcache")))
	}

	// setup config provider
	ret.provider = getProviderImpl(s, statsManager, ret.store)

	// setup http router
	ret.router = mux.NewRouter()

	// setup healthcheck path
	ret.health = NewHealthChecker(health.NewServer(), name, s.HealthyWithAtLeastOneConfigLoaded)
	ret.router.Path("/healthcheck").Handler(ret.health)
	healthpb.RegisterHealthServer(ret.grpcServer, ret.health.Server())

	// setup default debug listener
	ret.debugListener.debugMux = http.NewServeMux()
	ret.debugListener.endpoints = map[string]string{}
	ret.AddDebugHttpEndpoint(
		"/debug/pprof/",
		"root of various pprof endpoints. hit for help.",
		func(writer http.ResponseWriter, request *http.Request) {
			pprof.Index(writer, request)
		})

	// setup cpu profiling endpoint
	ret.AddDebugHttpEndpoint(
		"/debug/pprof/profile",
		"CPU profiling endpoint",
		func(writer http.ResponseWriter, request *http.Request) {
			pprof.Profile(writer, request)
		})

	// setup stats endpoint
	ret.AddDebugHttpEndpoint(
		"/stats",
		"print out stats",
		func(writer http.ResponseWriter, request *http.Request) {
			expvar.Do(func(kv expvar.KeyValue) {
				io.WriteString(writer, fmt.Sprintf("%s: %s\n", kv.Key, kv.Value))
			})
		})

	// setup trace endpoint
	ret.AddDebugHttpEndpoint(
		"/debug/pprof/trace",
		"trace endpoint",
		func(writer http.ResponseWriter, request *http.Request) {
			pprof.Trace(writer, request)
		})

	// setup debug root
	ret.debugListener.debugMux.HandleFunc(
		"/",
		func(writer http.ResponseWriter, request *http.Request) {
			sortedKeys := []string{}
			for key := range ret.debugListener.endpoints {
				sortedKeys = append(sortedKeys, key)
			}

			sort.Strings(sortedKeys)
			for _, key := range sortedKeys {
				io.WriteString(
					writer, fmt.Sprintf("%s: %s\n", key, ret.debugListener.endpoints[key]))
			}
		})

	return ret
}

func (server *server) Stop() {
	server.grpcServer.GracefulStop()
	server.listenerMu.Lock()
	defer server.listenerMu.Unlock()
	if server.debugListener.listener != nil {
		server.debugListener.listener.Close()
	}
	if server.httpServer != nil {
		server.httpServer.Close()
	}
	server.provider.Stop()
}

func (server *server) handleGracefulShutdown() {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)

	go func() {
		sig := <-sigs

		logger.Info(context.Background(), fmt.Sprintf("Ratelimit server received %v, shutting down gracefully", sig))
		server.Stop()
		os.Exit(0)
	}()
}

func (server *server) HealthChecker() *HealthChecker {
	return server.health
}
