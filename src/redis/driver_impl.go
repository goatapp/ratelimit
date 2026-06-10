package redis

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"strings"
	"time"

	"github.com/jpillora/backoff"
	stats "github.com/lyft/gostats"
	"github.com/mediocregopher/radix/v4"
	"github.com/mediocregopher/radix/v4/trace"
	logger "github.com/goatapp/ratelimit/src/log"

	"github.com/goatapp/ratelimit/src/server"
	"github.com/goatapp/ratelimit/src/utils"
)

type poolStats struct {
	connectionActive stats.Gauge
	connectionTotal  stats.Counter
	connectionClose  stats.Counter
}

func newPoolStats(scope stats.Scope) poolStats {
	ret := poolStats{}
	ret.connectionActive = scope.NewGauge("cx_active")
	ret.connectionTotal = scope.NewCounter("cx_total")
	ret.connectionClose = scope.NewCounter("cx_local_close")
	return ret
}

func poolTrace(ps *poolStats, healthCheckActiveConnection bool, srv server.Server) trace.PoolTrace {
	return trace.PoolTrace{
		ConnCreated: func(newConn trace.PoolConnCreated) {
			if newConn.Err == nil {
				ps.connectionTotal.Add(1)
				ps.connectionActive.Add(1)
				if healthCheckActiveConnection && srv != nil {
					err := srv.HealthChecker().Ok(server.RedisHealthComponentName)
					if err != nil {
						logger.Error(context.Background(), fmt.Sprintf("Unable to update health status: %s", err))
					}
				}
			} else {
				logger.Error(context.Background(), fmt.Sprintf("creating redis connection error : %v", newConn.Err))
			}
		},
		ConnClosed: func(_ trace.PoolConnClosed) {
			ps.connectionActive.Sub(1)
			ps.connectionClose.Add(1)
			if healthCheckActiveConnection && srv != nil && ps.connectionActive.Value() == 0 {
				err := srv.HealthChecker().Fail(server.RedisHealthComponentName)
				if err != nil {
					logger.Error(context.Background(), fmt.Sprintf("Unable to update health status: %s", err))
				}
			}
		},
	}
}

// redisClient is an interface that abstracts radix Client, Cluster, and Sentinel
// All of these types have Do(context.Context, Action) and Close() methods
type redisClient interface {
	Do(context.Context, radix.Action) error
	Close() error
}

type clientImpl struct {
	client                     redisClient
	stats                      poolStats
	implicitPipelining         bool
	isCluster                  bool
	clusterPipelineParallelism int
}

func checkError(err error) {
	if err != nil {
		panic(RedisError(err.Error()))
	}
}

func effectiveClusterPipelineParallelism(configuredParallelism, poolSize int) int {
	if configuredParallelism < 0 {
		panic(RedisError("redis cluster pipeline parallelism must be >= 0"))
	}

	if configuredParallelism == 1 {
		return 1
	}

	poolCeiling := poolSize
	if poolCeiling < 1 {
		poolCeiling = 1
	}

	if configuredParallelism == 0 || configuredParallelism > poolCeiling {
		return poolCeiling
	}

	return configuredParallelism
}

// createDialer creates a radix.Dialer with timeout, TLS, and auth configuration
// targetName is used for logging to identify the connection target (e.g., URL, "sentinel(url)")
func createDialer(timeout time.Duration, useTls bool, tlsConfig *tls.Config, auth string, targetName string) radix.Dialer {
	var netDialer net.Dialer
	if timeout > 0 {
		netDialer.Timeout = timeout
	}

	dialer := radix.Dialer{
		NetDialer: &netDialer,
	}

	// Setup TLS if needed
	if useTls {
		tlsNetDialer := tls.Dialer{
			NetDialer: &netDialer,
			Config:    tlsConfig,
		}
		dialer.NetDialer = &tlsNetDialer
		if targetName != "" {
			logger.Warn(context.Background(), fmt.Sprintf("enabling TLS to redis %s", targetName))
		}
	}

	// Setup auth if provided
	if auth != "" {
		user, pass, found := strings.Cut(auth, ":")
		if found {
			logger.Warn(context.Background(), fmt.Sprintf("enabling authentication to redis %s with user %s", targetName, user))
			dialer.AuthUser = user
			dialer.AuthPass = pass
		} else {
			logger.Warn(context.Background(), fmt.Sprintf("enabling authentication to redis %s without user", targetName))
			dialer.AuthPass = auth
		}
	}

	return dialer
}

func NewClientImpl(ctx context.Context, scope stats.Scope, useTls bool, auth, redisSocketType, redisType, url string, poolSize int,
	pipelineWindow time.Duration, pipelineLimit int, tlsConfig *tls.Config, healthCheckActiveConnection bool, srv server.Server,
	timeout time.Duration, poolOnEmptyBehavior string, sentinelAuth string,
	startupInitialInterval, startupMaxInterval, startupMaxElapsedTime time.Duration,
) Client {
	return newClientImpl(ctx, scope, useTls, auth, redisSocketType, redisType, url, poolSize,
		pipelineWindow, pipelineLimit, tlsConfig, healthCheckActiveConnection, srv,
		timeout, poolOnEmptyBehavior, sentinelAuth,
		startupInitialInterval, startupMaxInterval, startupMaxElapsedTime, 1)
}

func newClientImpl(ctx context.Context, scope stats.Scope, useTls bool, auth, redisSocketType, redisType, url string, poolSize int,
	pipelineWindow time.Duration, pipelineLimit int, tlsConfig *tls.Config, healthCheckActiveConnection bool, srv server.Server,
	timeout time.Duration, poolOnEmptyBehavior string, sentinelAuth string,
	startupInitialInterval, startupMaxInterval, startupMaxElapsedTime time.Duration,
	clusterPipelineParallelism int,
) Client {
	maskedUrl := utils.MaskCredentialsInUrl(url)
	logger.Warn(context.Background(), fmt.Sprintf("connecting to redis on %s with pool size %d", maskedUrl, poolSize))

	// Create Dialer for connecting to Redis
	dialer := createDialer(timeout, useTls, tlsConfig, auth, maskedUrl)

	stats := newPoolStats(scope)

	// Create PoolConfig
	poolConfig := radix.PoolConfig{
		Dialer: dialer,
		Size:   poolSize,
		Trace:  poolTrace(&stats, healthCheckActiveConnection, srv),
	}

	// Determine pipeline mode based on Redis type:
	// - Cluster: uses grouped pipeline (same-key commands batched together)
	// - Single/Sentinel: uses explicit pipeline (all commands batched together)
	isCluster := strings.ToLower(redisType) == "cluster"

	// pipelineLimit parameter is deprecated and ignored in radix v4.
	if pipelineLimit > 0 {
		logger.Warn(context.Background(), fmt.Sprintf("REDIS_PIPELINE_LIMIT=%d is deprecated and has no effect in radix v4. Write buffering is controlled solely by REDIS_PIPELINE_WINDOW.", pipelineLimit))
	}

	// Set WriteFlushInterval for cluster mode (grouped pipeline uses auto buffering)
	if isCluster && pipelineWindow > 0 {
		poolConfig.Dialer.WriteFlushInterval = pipelineWindow
		logger.Debug(context.Background(), fmt.Sprintf("Cluster mode: setting WriteFlushInterval to %v", pipelineWindow))
	}

	effectivePipelineParallelism := clusterPipelineParallelism
	if isCluster {
		effectivePipelineParallelism = effectiveClusterPipelineParallelism(clusterPipelineParallelism, poolSize)
		switch {
		case clusterPipelineParallelism == 0:
			logger.Warn(context.Background(), fmt.Sprintf("Redis cluster pipeline parallelism: auto bounded to Redis pool size (%d concurrent groups)", effectivePipelineParallelism))
		case clusterPipelineParallelism != effectivePipelineParallelism:
			logger.Warn(context.Background(), fmt.Sprintf("Redis cluster pipeline parallelism: configured value %d exceeds Redis pool size %d; bounded to %d concurrent groups", clusterPipelineParallelism, poolSize, effectivePipelineParallelism))
		case effectivePipelineParallelism == 1:
			logger.Warn(context.Background(), "Redis cluster pipeline parallelism: disabled (serial legacy behavior)")
		default:
			logger.Warn(context.Background(), fmt.Sprintf("Redis cluster pipeline parallelism: bounded to %d concurrent groups", effectivePipelineParallelism))
		}
	}

	// IMPORTANT: radix v4 pool behavior changes from v3
	//
	// v4 uses a FIXED pool size and BLOCKS when all connections are in use.
	// This is the same as v3's WAIT behavior.
	//
	// v3 CREATE and ERROR behaviors are NOT supported in v4:
	// - v3 WAIT   → v4 supported (blocks until connection available)
	// - v3 CREATE → v4 NOT SUPPORTED (would block instead of creating overflow connections)
	// - v3 ERROR  → v4 NOT SUPPORTED (would block instead of failing fast)
	//
	// Migration requirements:
	// - Remove REDIS_POOL_ON_EMPTY_BEHAVIOR setting if set to CREATE or ERROR
	// - Use WAIT or leave unset (WAIT is default)
	// - Consider increasing REDIS_POOL_SIZE if you previously relied on CREATE
	// - Use context timeouts to prevent indefinite blocking:
	//     ctx, cancel := context.WithTimeout(context.Background(), 1*time.Second)
	//     defer cancel()
	//     client.Do(ctx, cmd)
	switch strings.ToUpper(poolOnEmptyBehavior) {
	case "WAIT":
		logger.Warn(context.Background(), fmt.Sprintf("Redis pool %s: WAIT is default in radix v4 (blocks until connection available)", maskedUrl))
	case "CREATE":
		// v3 CREATE created overflow connections when pool was full
		// v4 does NOT support this - fail fast to prevent unexpected blocking behavior
		panic(RedisError("REDIS_POOL_ON_EMPTY_BEHAVIOR=CREATE is not supported in radix v4. Pool will block instead of creating overflow connections. Remove this setting or set to WAIT, and consider increasing REDIS_POOL_SIZE."))
	case "ERROR":
		// v3 ERROR failed fast when pool was full
		// v4 does NOT support this - fail fast to prevent unexpected blocking behavior
		panic(RedisError("REDIS_POOL_ON_EMPTY_BEHAVIOR=ERROR is not supported in radix v4. Pool will block instead of failing fast. Remove this setting or set to WAIT, and use context timeouts for fail-fast behavior."))
	default:
		logger.Warn(context.Background(), fmt.Sprintf("Redis pool %s: using v4 default (fixed size=%d, blocks when full)", maskedUrl, poolSize))
	}

	poolFunc := func(ctx context.Context, network, addr string) (radix.Client, error) {
		return poolConfig.New(ctx, network, addr)
	}

	// Validate sentinel URL format early (before retry loop) since it's a configuration error.
	if strings.ToLower(redisType) == "sentinel" {
		urls := strings.Split(url, ",")
		if len(urls) < 2 {
			panic(RedisError("Expected master name and a list of urls for the sentinels, in the format: <redis master name>,<sentinel1>,...,<sentineln>"))
		}
	}

	b := &backoff.Backoff{
		Min:    startupInitialInterval,
		Max:    startupMaxInterval,
		Factor: 2,
		Jitter: true,
	}

	startTime := time.Now()

	retryOrDie := func(lastErr error) {
		elapsed := time.Since(startTime)
		if startupMaxElapsedTime > 0 && elapsed >= startupMaxElapsedTime {
			panic(RedisError(fmt.Sprintf("timed out waiting for Redis connection to %s after %s: %v", maskedUrl, elapsed.Round(time.Millisecond), lastErr)))
		}
		d := b.Duration()
		logger.Warn(context.Background(), fmt.Sprintf("Retrying Redis connection to %s in %s (elapsed: %s): %v", maskedUrl, d, elapsed.Round(time.Millisecond), lastErr))
		select {
		case <-time.After(d):
		case <-ctx.Done():
			panic(RedisError(fmt.Sprintf("context cancelled while waiting for Redis connection to %s: %v", maskedUrl, ctx.Err())))
		}
	}

	var client redisClient
	for {
		var err error
		switch strings.ToLower(redisType) {
		case "single":
			logger.Warn(context.Background(), fmt.Sprintf("Creating single with urls %v", url))
			client, err = poolFunc(ctx, redisSocketType, url)
		case "cluster":
			urls := strings.Split(url, ",")
			logger.Warn(context.Background(), fmt.Sprintf("Creating cluster with urls %v", urls))
			clusterConfig := radix.ClusterConfig{
				PoolConfig: poolConfig,
			}
			client, err = clusterConfig.New(ctx, urls)
		case "sentinel":
			urls := strings.Split(url, ",")
			sentinelDialer := createDialer(timeout, useTls, tlsConfig, sentinelAuth, fmt.Sprintf("sentinel(%s)", maskedUrl))
			sentinelConfig := radix.SentinelConfig{
				PoolConfig:     poolConfig,
				SentinelDialer: sentinelDialer,
			}
			client, err = sentinelConfig.New(ctx, urls[0], urls[1:])
		default:
			panic(RedisError("Unrecognized redis type " + redisType))
		}

		if err != nil {
			retryOrDie(err)
			continue
		}

		var pingResponse string
		if pingErr := client.Do(ctx, radix.Cmd(&pingResponse, "PING")); pingErr != nil {
			_ = client.Close()
			retryOrDie(pingErr)
			continue
		}
		if pingResponse != "PONG" {
			_ = client.Close()
			retryOrDie(fmt.Errorf("unexpected PING response: %q", pingResponse))
			continue
		}

		// Successfully connected.
		break
	}

	if srv != nil {
		if err := srv.HealthChecker().Ok(server.RedisHealthComponentName); err != nil {
			logger.Error(context.Background(), fmt.Sprintf("Unable to update health status after Redis connection: %s", err))
		}
	}

	return &clientImpl{
		client:                     client,
		stats:                      stats,
		implicitPipelining:         pipelineWindow > 0 || isCluster,
		isCluster:                  isCluster,
		clusterPipelineParallelism: effectivePipelineParallelism,
	}
}

func (c *clientImpl) DoCmd(ctx context.Context, rcv interface{}, cmd string, args ...interface{}) error {
	return c.client.Do(ctx, radix.FlatCmd(rcv, cmd, args...))
}

func (c *clientImpl) Close() error {
	return c.client.Close()
}

func (c *clientImpl) NumActiveConns() int {
	return int(c.stats.connectionActive.Value())
}

func (c *clientImpl) PipeAppend(pipeline Pipeline, rcv interface{}, cmd string, args ...interface{}) Pipeline {
	return append(pipeline, radix.FlatCmd(rcv, cmd, args...))
}

func (c *clientImpl) PipeScriptAppend(pipeline Pipeline, rcv interface{}, script radix.EvalScript, args ...string) Pipeline {
	return append(pipeline, script.FlatCmd(rcv, nil, args))
}

func (c *clientImpl) ImplicitPipeliningEnabled() bool {
	return c.implicitPipelining
}

func (c *clientImpl) PipeDo(ctx context.Context, pipeline Pipeline) error {
	for _, action := range pipeline {
		if err := c.client.Do(ctx, action); err != nil {
			return err
		}
	}
	return nil
}

