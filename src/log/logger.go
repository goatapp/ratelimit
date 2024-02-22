package log

import (
	"context"
	"encoding/json"
	"maps"
	"os"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/metadata"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
)

const (
	loggerSettingsJson = `{
		"level": "debug",
		"encoding": "console",
		"outputPaths": ["stderr"],
	  	"errorOutputPaths": ["stderr"],
		"development": false,
		"disableStacktrace": true,
	  	"encoderConfig": {
	    	"messageKey": "message",
	    	"levelKey": "level",
	    	"levelEncoder": "capital",
			"timeKey": "timestamp",
			"timeEncoder": "iso8601",
			"callerKey": "caller",
			"callerEncoder": "short",
			"stacktraceKey": "stack"
	  	}
	}`
)

type LogLevel string

const (
	DEBUG LogLevel = "DEBUG"
	ERROR LogLevel = "ERROR"
	WARN  LogLevel = "WARN"
	INFO  LogLevel = "INFO"
)

type LogFormat string

const (
	CONSOLE LogFormat = "console"
	JSON    LogFormat = "json"
)

type Log struct {
	Level      LogLevel
	Time       time.Time
	LoggerName string
	Message    string
	Caller     string
	Stack      string
}

var (
	contextParsers      []ContextParser
	contextParsersMutex sync.RWMutex
	l                   *zap.Logger
)

const (
	correlationIdKey   = "correlation_id"
	ipAddressKey       = "ip_address"
	originUserAgentKey = "origin_user_agent"
	pathKey            = "pathKey"
	requestIdKey       = "request_id"
	userAgentKey       = "user_agent"
)

var (
	loggableGRPCMetadata = map[string]string{
		"x-goat-correlation-id":  correlationIdKey,
		"x-forwarded-for":        ipAddressKey,
		"x-forwarded-user-agent": originUserAgentKey,
		"x-request-id":           requestIdKey,
		"user-agent":             userAgentKey,
	}
)

// nolint: gochecknoinits
func init() {
	l = New(
		LogFormat(os.Getenv("LOG_FORMAT")),
		LogLevel(os.Getenv("LOG_LEVEL")),
	)

	RegisterContextParser(scopedFieldsContextParser)
	RegisterContextParser(grpcContextLogParser)
}

func withStackTrace(fields []zapcore.Field) []zapcore.Field {
	return append(fields, zap.Stack("stacktrace"))
}

type LogField struct {
	Key   string
	Value interface{}
}

type LogOptions struct {
	Err     error
	Fields  []LogField
	Context context.Context
}

type LogOption func(*LogOptions)

func newOptionFields(ctx context.Context, errOpts ...LogOption) []zapcore.Field {
	var fields []zapcore.Field
	opts := &LogOptions{}

	var ctxOpts []LogOption

	contextParsersMutex.RLock()
	defer contextParsersMutex.RUnlock()

	for _, parser := range contextParsers {
		ctxOpts = append(ctxOpts, parser(ctx)...)
	}

	// nolint: gocritic
	augmentedOpts := append(errOpts, ctxOpts...)

	for _, option := range augmentedOpts {
		option(opts)
	}

	for _, field := range opts.Fields {
		fields = append(fields, zap.Reflect(field.Key, field.Value))
	}

	if opts.Err != nil {
		fields = append(fields, zap.Error(opts.Err))
	}

	return fields
}

func WithValue(key string, value interface{}) LogOption {
	return func(opts *LogOptions) {
		opts.Fields = append(opts.Fields, LogField{
			Key:   key,
			Value: value,
		})
	}
}

func WithError(err error) LogOption {
	return func(opts *LogOptions) {
		opts.Err = err
	}
}

func New(env LogFormat, logLevel LogLevel) *zap.Logger {
	var cfg zap.Config
	if err := json.Unmarshal([]byte(loggerSettingsJson), &cfg); err != nil {
		panic(err)
	}

	if env == JSON {
		cfg.Encoding = "json"
	}

	switch logLevel {
	case DEBUG:
		cfg.Level.SetLevel(zap.DebugLevel)
	case ERROR:
		cfg.Level.SetLevel(zap.ErrorLevel)
	case WARN:
		cfg.Level.SetLevel(zap.WarnLevel)
	case INFO:
		cfg.Level.SetLevel(zap.InfoLevel)
	default:
		cfg.Level.SetLevel(zap.InfoLevel)
	}

	logger, err := cfg.Build()
	if err != nil {
		panic(err)
	}

	l = logger.WithOptions(zap.AddCallerSkip(1))

	return l
}

func Debug(ctx context.Context, msg string, opts ...LogOption) {
	l.Debug(msg, newOptionFields(ctx, opts...)...)
}

func Info(ctx context.Context, msg string, opts ...LogOption) {
	l.Info(msg, newOptionFields(ctx, opts...)...)
}

func Warn(ctx context.Context, msg string, opts ...LogOption) {
	l.Warn(msg, newOptionFields(ctx, opts...)...)
}

func Error(ctx context.Context, msg string, opts ...LogOption) {
	l.Error(msg, withStackTrace(newOptionFields(ctx, opts...))...)
}

func Fatal(ctx context.Context, msg string, opts ...LogOption) {
	l.Fatal(msg, withStackTrace(newOptionFields(ctx, opts...))...)
}

func Panic(ctx context.Context, msg string, opts ...LogOption) {
	l.Panic(msg, withStackTrace(newOptionFields(ctx, opts...))...)
}

func DPanic(ctx context.Context, msg string, opts ...LogOption) {
	l.DPanic(msg, withStackTrace(newOptionFields(ctx, opts...))...)
}

func Sync() {
	_ = l.Sync()
}

type scopedFieldsCtxKey struct{}

func (k *scopedFieldsCtxKey) String() string { return "go-services/common/log scoped fields" }

// AddField adds a field to be logged along with the context. If the field already exists, it will be overwritten.
func AddField(ctx context.Context, key string, value interface{}) context.Context {
	if key == "" {
		return ctx
	}

	return AddFields(ctx, LogField{Key: key, Value: value})
}

// AddFields adds fields to be logged along with the context. If a field already exists, it will be overwritten.
func AddFields(ctx context.Context, fields ...LogField) context.Context {
	if len(fields) == 0 {
		return ctx
	}

	var fieldMap map[string]interface{}

	if existing, ok := ctx.Value(scopedFieldsCtxKey{}).(map[string]interface{}); ok {
		fieldMap = maps.Clone(existing)
	} else {
		fieldMap = make(map[string]interface{})
	}

	for _, field := range fields {
		fieldMap[field.Key] = field.Value
	}

	return context.WithValue(ctx, scopedFieldsCtxKey{}, fieldMap)
}

func grpcContextLogParser(ctx context.Context) []LogOption {
	var logOptions []LogOption

	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return logOptions
	}

	for mdKey, logKey := range loggableGRPCMetadata {
		if mdVals := md.Get(mdKey); len(mdVals) > 0 {
			logOptions = append(logOptions, WithValue(logKey, mdVals[0]))
		}
	}

	method, ok := grpc.Method(ctx)
	if ok {
		// Method looks something like - goat.protos.Api/Rpc
		splitMethod := strings.Split(method, ".")

		// Get the last value - Api/Rpc
		method = splitMethod[len(splitMethod)-1]
		// Do a char replacement to . to properly index - Api.Rpc
		method = strings.ReplaceAll(method, "/", ".")

		logOptions = append(logOptions, WithValue(pathKey, method))
	}

	return logOptions
}

func scopedFieldsContextParser(ctx context.Context) []LogOption {
	var opts []LogOption

	if userFieldMap, ok := ctx.Value(scopedFieldsCtxKey{}).(map[string]interface{}); ok {
		for key, value := range userFieldMap {
			opts = append(opts, WithValue(key, value))
		}
	}

	return opts
}

type ContextParser func(ctx context.Context) []LogOption

func RegisterContextParser(f ContextParser) {
	contextParsersMutex.Lock()
	defer contextParsersMutex.Unlock()

	contextParsers = append(contextParsers, f)
}
