package keygen

import "fmt"

const prefix = "rl"	// all our Redis keys start with "rl:" — easy to spot in Redis CLI

// FromIP builds a key for IP-based limiting.
// e.g. "rl:ip:203.0.113.42"
func FromIP(ip string) string {
	return fmt.Sprintf("%s:ip:%s", prefix, ip)
}

// FromUser builds a key for user/API-key-based limiting.
// e.g. "rl:user:abc123"
func FromUser(userID string) string {
	return fmt.Sprintf("%s:user:%s", prefix, userID)
}

// FromRoute builds a key for route-based limiting.
// e.g. "rl:route:/api/v1/login"
func FromRoute(route string) string {
	return fmt.Sprintf("%s:route:%s", prefix, route)
}

// FromRouteAndUser builds a composite key for per-user per-route limiting.
// e.g. "rl:route:/api/v1/login:user:abc123"
func FromRouteAndUser(route, userID string) string {
	return fmt.Sprintf("%s:route:%s:user:%s", prefix, route, userID)
}


// FromRouteAndIP builds a composite key for per-IP per-route limiting.
// e.g. "rl:route:/api/v1/login:ip:203.0.113.42"
func FromRouteAndIP(route, ip string) string {
	return fmt.Sprintf("%s:route:%s:ip:%s", prefix, route, ip)
}