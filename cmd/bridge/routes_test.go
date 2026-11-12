package main

import (
	"strings"
	"testing"

	"github.com/longern/webtransport-bridge/internal/bridge"
)

func TestRouteConfigRejectsUnsafeOrAmbiguousRules(t *testing.T) {
	const rule = `  - matches:
      - path: { type: Exact, value: /webtransport/photos }
        headers: [{ type: Exact, name: Origin, value: https://photos.example }]
    backendRefs: [{ name: photos }]
    streamPolicy:
      public: true
      allowedPaths: [/dav]
`
	const config = "version: 1\nbackends:\n  photos: http://nas:8080\nrules:\n" + rule
	for _, tc := range []struct{ name, input, want string }{
		{"duplicate match", config + rule, "duplicate match"},
		{"missing Origin condition", strings.ReplaceAll(config, "name: Origin", "name: X-Origin"), "exactly one Origin"},
		{"origin wildcard", strings.ReplaceAll(config, "https://photos.example", "https://*.example"), "exact HTTPS origin"},
		{"origin regex", strings.ReplaceAll(config, "headers: [{ type: Exact", "headers: [{ type: RegularExpression"), "exact HTTPS origin"},
		{"path prefix", strings.ReplaceAll(config, "path: { type: Exact", "path: { type: PathPrefix"), "Exact URL path"},
		{"ambiguous path", strings.ReplaceAll(config, "/webtransport/photos", "/webtransport//photos"), "Exact URL path"},
		{"undefined backend", strings.ReplaceAll(config, "name: photos", "name: missing"), "unknown backend"},
		{"multiple backends", strings.ReplaceAll(config, "{ name: photos }]", "{ name: photos }, { name: photos }]"), "exactly one backendRef"},
		{"backend with credentials", strings.ReplaceAll(config, "http://nas", "http://user:password@nas"), "valid HTTP(S) origin"},
		{"unknown policy field", strings.ReplaceAll(config, "allowedPaths", "allowedPath"), "field allowedPath not found"},
		{"explicit empty allowlist", strings.ReplaceAll(config, "[/dav]", "[]"), "allowedPaths must not be empty"},
		{"protected route without token", strings.ReplaceAll(config, "public: true", "tokenEnv: PHOTOS_TOKEN"), "set PHOTOS_TOKEN"},
		{"public with token", strings.ReplaceAll(config, "public: true", "public: true\n      tokenEnv: PHOTOS_TOKEN"), "cannot be combined"},
		{"extra YAML document", config + "---\nversion: 1\n", "exactly one YAML document"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := decodeRoutes(strings.NewReader(tc.input), bridge.Bridge{}, func(string) string { return "" })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("decodeRoutes error = %v; want %q", err, tc.want)
			}
		})
	}
}
