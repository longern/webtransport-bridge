package main

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path"
	"regexp"
	"strings"
	"unicode"

	"github.com/longern/webtransport-bridge/internal/bridge"
	"go.yaml.in/yaml/v3"
)

const (
	defaultSessionPath = "/webtransport"
	defaultMethods     = "GET,HEAD,POST,PUT"
	defaultTokenEnv    = "BRIDGE_TOKEN"
)

// Keys are the exact CONNECT path and the browser's serialized Origin.
type sessionRoutes map[string]originRoutes

func (routes sessionRoutes) checkOrigin(r *http.Request) bool {
	return routes.originsFor(r).backendFor(r) != nil
}

func (routes sessionRoutes) originsFor(r *http.Request) originRoutes {
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		return nil
	}
	return routes[r.URL.EscapedPath()]
}

type originRoutes map[string]*bridge.Bridge

func (routes originRoutes) backendFor(r *http.Request) *bridge.Bridge {
	origins := r.Header.Values("Origin")
	if len(origins) != 1 {
		return nil
	}
	return routes[origins[0]]
}

type routeConfig struct {
	Version  int               `yaml:"version"`
	Backends map[string]string `yaml:"backends"`
	Rules    []routeRule       `yaml:"rules"`
}

type routeRule struct {
	Matches     []routeMatch `yaml:"matches"`
	BackendRefs []backendRef `yaml:"backendRefs"`
	Policy      streamPolicy `yaml:"streamPolicy"`
}

type routeMatch struct {
	Path    exactMatch    `yaml:"path"`
	Headers []headerMatch `yaml:"headers"`
}

type exactMatch struct {
	Type  string `yaml:"type"`
	Value string `yaml:"value"`
}

type headerMatch struct {
	Name  string `yaml:"name"`
	Type  string `yaml:"type"`
	Value string `yaml:"value"`
}

type backendRef struct {
	Name string `yaml:"name"`
}

type streamPolicy struct {
	AllowedPaths   []string `yaml:"allowedPaths"`
	AllowedMethods []string `yaml:"allowedMethods"`
	Public         bool     `yaml:"public"`
	TokenEnv       string   `yaml:"tokenEnv"`
}

func readRoutes(filename string, base bridge.Bridge) (sessionRoutes, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	return decodeRoutes(file, base, os.Getenv)
}

func decodeRoutes(r io.Reader, base bridge.Bridge, getenv func(string) string) (sessionRoutes, error) {
	var config routeConfig
	decoder := yaml.NewDecoder(r)
	decoder.KnownFields(true)
	if err := decoder.Decode(&config); err != nil {
		return nil, fmt.Errorf("decode route configuration: %w", err)
	}
	var extra yaml.Node
	if err := decoder.Decode(&extra); !errors.Is(err, io.EOF) {
		return nil, errors.New("route configuration must contain exactly one YAML document")
	}
	if config.Version != 1 {
		return nil, errors.New("route configuration requires version: 1")
	}
	if len(config.Rules) == 0 || len(config.Backends) == 0 {
		return nil, errors.New("route configuration requires backends and rules")
	}
	backends := make(map[string]*url.URL, len(config.Backends))
	for name, value := range config.Backends {
		backend, err := parseBackend(value)
		if name == "" || err != nil {
			return nil, fmt.Errorf("backend %q must have a name and a valid HTTP(S) origin without credentials, path, query, or fragment", name)
		}
		backends[name] = backend
	}
	routes := make(sessionRoutes)
	for i, rule := range config.Rules {
		if err := routes.addRule(rule, backends, base, getenv); err != nil {
			return nil, fmt.Errorf("rule %d: %w", i+1, err)
		}
	}
	return routes, nil
}

func (routes sessionRoutes) addRule(rule routeRule, backends map[string]*url.URL, base bridge.Bridge, getenv func(string) string) error {
	if len(rule.Matches) == 0 || len(rule.BackendRefs) != 1 {
		return errors.New("expected at least one match and exactly one backendRef")
	}
	name := rule.BackendRefs[0].Name
	base.Backend = backends[name]
	if base.Backend == nil {
		return fmt.Errorf("unknown backend %q", name)
	}
	if err := rule.Policy.apply(&base, getenv); err != nil {
		return err
	}
	for _, match := range rule.Matches {
		p, origin, err := match.validate()
		if err != nil {
			return err
		}
		if routes[p] == nil {
			routes[p] = make(originRoutes)
		}
		if routes[p][origin] != nil {
			return fmt.Errorf("duplicate match for path %q and Origin %q", p, origin)
		}
		routes[p][origin] = &base
	}
	return nil
}

func (m routeMatch) validate() (string, string, error) {
	p := m.Path.Value
	if m.Path.Type != "Exact" || !validPath(p) || (&url.URL{Path: p}).EscapedPath() != p {
		return "", "", errors.New("matches require an Exact URL path without escapes")
	}
	if len(m.Headers) != 1 || !strings.EqualFold(m.Headers[0].Name, "Origin") {
		return "", "", errors.New("matches require exactly one Origin header condition")
	}
	match := m.Headers[0]
	if (match.Type != "" && match.Type != "Exact") || !validOrigin(match.Value) {
		return "", "", errors.New("Origin conditions must match an exact HTTPS origin without a trailing slash")
	}
	return p, match.Value, nil
}

var (
	methodToken = regexp.MustCompile("^[!#$%&'*+.^_`|~0-9A-Za-z-]+$")
	envName     = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

func (policy streamPolicy) apply(b *bridge.Bridge, getenv func(string) string) error {
	b.Prefixes = policy.AllowedPaths
	if b.Prefixes == nil {
		b.Prefixes = []string{"/"}
	}
	if len(b.Prefixes) == 0 {
		return errors.New("streamPolicy.allowedPaths must not be empty")
	}
	for _, prefix := range b.Prefixes {
		if !validPath(prefix) || strings.ContainsAny(prefix, "%?#") {
			return fmt.Errorf("invalid stream path prefix %q", prefix)
		}
	}
	methods := policy.AllowedMethods
	if methods == nil {
		methods = strings.Split(defaultMethods, ",")
	}
	if len(methods) == 0 {
		return errors.New("streamPolicy.allowedMethods must not be empty")
	}
	b.Methods = make(map[string]bool, len(methods))
	for _, method := range methods {
		if !methodToken.MatchString(method) || method == http.MethodConnect || (method == "*" && len(methods) != 1) {
			return fmt.Errorf("invalid stream method %q", method)
		}
		b.Methods[method] = true
	}
	if policy.Public {
		if policy.TokenEnv != "" {
			return errors.New("streamPolicy.public and tokenEnv cannot be combined")
		}
		b.Token = ""
		return nil
	}
	name := policy.TokenEnv
	if name == "" {
		name = defaultTokenEnv
	}
	if !envName.MatchString(name) {
		return errors.New("streamPolicy.tokenEnv must be an environment variable name")
	}
	b.Token = getenv(name)
	if b.Token == "" {
		return fmt.Errorf("set %s or explicitly enable streamPolicy.public", name)
	}
	return nil
}

func parseBackend(value string) (*url.URL, error) {
	u, err := url.Parse(value)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Hostname() == "" || u.User != nil || u.ForceQuery || u.RawQuery != "" || strings.Contains(value, "#") || (u.EscapedPath() != "" && u.EscapedPath() != "/") {
		return nil, errors.New("expected an HTTP(S) origin without credentials, path, query, or fragment")
	}
	return u, nil
}

func validOrigin(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Hostname() != "" && !strings.Contains(u.Host, "*") && value == "https://"+u.Host
}

func validPath(value string) bool {
	return strings.HasPrefix(value, "/") && !strings.Contains(value, "//") && !strings.Contains(value, "\\") &&
		strings.IndexFunc(value, unicode.IsControl) == -1 &&
		(value == "/" || path.Clean(value) == strings.TrimSuffix(value, "/"))
}

func legacyRoutes(base bridge.Bridge, backend, origins, paths, methods, token string, public bool) (sessionRoutes, error) {
	var err error
	base.Backend, err = parseBackend(backend)
	if err != nil {
		return nil, fmt.Errorf("--backend: %w", err)
	}
	base.Token, err = configuredToken(token, public)
	if err != nil {
		return nil, err
	}
	base.Prefixes = strings.Split(paths, ",")
	for _, p := range base.Prefixes {
		if !strings.HasPrefix(p, "/") {
			return nil, errors.New("path prefixes must start with /")
		}
	}
	base.Methods = make(map[string]bool)
	for _, method := range strings.Split(methods, ",") {
		base.Methods[strings.TrimSpace(method)] = true
	}
	allowed := make(originRoutes)
	for _, origin := range strings.Split(origins, ",") {
		origin = strings.TrimSpace(origin)
		if !validOrigin(origin) {
			return nil, errors.New("origins must be exact HTTPS origins without trailing slash")
		}
		allowed[origin] = &base
	}
	return sessionRoutes{defaultSessionPath: allowed}, nil
}
