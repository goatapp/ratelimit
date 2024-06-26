package utils

import (
	"strings"

	pb "github.com/envoyproxy/go-control-plane/envoy/service/ratelimit/v3"
	"github.com/golang/protobuf/ptypes/duration"
)

// Interface for a time source.
type TimeSource interface {
	// @return the current unix time in milliseconds.
	UnixNow() int64
}

// Convert a rate limit into a time divider.
// @param unit supplies the unit to convert.
// @return the divider to use in time computations.
func UnitToDivider(unit pb.RateLimitResponse_RateLimit_Unit) int64 {
	switch unit {
	case pb.RateLimitResponse_RateLimit_SECOND:
		return 1
	case pb.RateLimitResponse_RateLimit_MINUTE:
		return 60
	case pb.RateLimitResponse_RateLimit_HOUR:
		return 60 * 60
	case pb.RateLimitResponse_RateLimit_DAY:
		return 60 * 60 * 24
	}

	panic("should not get here")
}

func CalculateReset(unit *pb.RateLimitResponse_RateLimit_Unit, timeSource TimeSource) *duration.Duration {
	sec := UnitToDivider(*unit)
	now := timeSource.UnixNow() / 1000
	return &duration.Duration{Seconds: sec - now%sec}
}

func Max(a uint32, b uint32) uint32 {
	if a > b {
		return a
	}
	return b
}

// Mask credentials from a redis connection string like
// foo,redis://user:pass@redisurl1,redis://user:pass@redisurl2
// resulting in
// foo,redis://*****@redisurl1,redis://*****@redisurl2
func MaskCredentialsInUrl(url string) string {
	urls := strings.Split(url, ",")

	for i := 0; i < len(urls); i++ {
		url := urls[i]
		authUrlParts := strings.Split(url, "@")
		if len(authUrlParts) > 1 && strings.HasPrefix(authUrlParts[0], "redis://") {
			urls[i] = "redis://*****@" + authUrlParts[len(authUrlParts)-1]
		}
	}

	return strings.Join(urls, ",")
}

// Remove invalid characters from the stat name.
func SanitizeStatName(s string) string {
	r := strings.NewReplacer(":", "_", "|", "_")
	return r.Replace(s)
}
