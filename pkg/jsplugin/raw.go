package jsplugin

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

type RawRoute struct {
	Name    string `json:"name"`
	BaseURL string `json:"baseUrl"`
}

var rawNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

func decodeRawRoutes(value any) ([]RawRoute, error) {
	if value == nil {
		return nil, nil
	}
	values, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("rawRoutes must be an array")
	}
	routes := make([]RawRoute, 0, len(values))
	for _, item := range values {
		object, ok := item.(map[string]any)
		if !ok || len(object) != 2 {
			return nil, fmt.Errorf("rawRoutes entries require exactly name and baseUrl")
		}
		name, nameOK := object["name"].(string)
		base, baseOK := object["baseUrl"].(string)
		if !nameOK || !baseOK {
			return nil, fmt.Errorf("rawRoutes name and baseUrl must be strings")
		}
		routes = append(routes, RawRoute{Name: name, BaseURL: base})
	}
	return routes, normalizeRawRoutes(routes)
}

func normalizeRawRoutes(routes []RawRoute) error {
	seen := make(map[string]bool)
	for i := range routes {
		if !rawNamePattern.MatchString(routes[i].Name) || seen[routes[i].Name] {
			return fmt.Errorf("invalid or duplicate raw route name %q", routes[i].Name)
		}
		seen[routes[i].Name] = true
		normalized, err := normalizeMetaBaseURL(routes[i].BaseURL)
		if err != nil {
			return fmt.Errorf("raw route %s: %w", routes[i].Name, err)
		}
		parsed, _ := url.Parse(normalized)
		if parsed.Path != "" {
			return fmt.Errorf("raw route baseUrl must contain only scheme and authority")
		}
		routes[i].BaseURL = normalized
	}
	return nil
}

// LookupRawRoute holds the plugin pointer from the same immutable generation.
func (g *RoutingGeneration) LookupRawRoute(name string) (*LoadedPlugin, RawRoute, bool) {
	if g == nil {
		return nil, RawRoute{}, false
	}
	for _, plugin := range g.plugins {
		for _, route := range plugin.Meta.RawRoutes {
			if route.Name == name {
				return plugin, route, true
			}
		}
	}
	return nil, RawRoute{}, false
}

// RawTarget strips the escaped prefix without resolving or cleaning the remainder.
func RawTarget(route RawRoute, escapedPath, rawQuery string, forceQuery bool) (*url.URL, error) {
	suffix, ok := strings.CutPrefix(escapedPath, "/raw/"+route.Name+"/")
	if !ok {
		return nil, fmt.Errorf("raw path must use a literal route name")
	}
	target, err := url.Parse(route.BaseURL + "/" + suffix)
	if err != nil {
		return nil, fmt.Errorf("invalid raw path")
	}
	target.RawQuery, target.ForceQuery = rawQuery, forceQuery
	return target, nil
}
