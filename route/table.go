package route

import (
	"bytes"
	"errors"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/gobwas/glob"
)

var errInvalidPrefix = errors.New("route: prefix must not be empty")
var errInvalidTarget = errors.New("route: target must not be empty")
var errNoMatch = errors.New("route: no target match")

// table stores the active routing table. Must never be nil.
var table atomic.Value

// init initializes the routing table.
func init() {
	table.Store(make(Table))
}

// GetTable returns the active routing table. The function
// is safe to be called from multiple goroutines and the
// value is never nil.
func GetTable() Table {
	return table.Load().(Table)
}

// SetTable sets the active routing table. A nil value
// logs a warning and is ignored. The function is safe
// to be called from multiple goroutines.
func SetTable(t Table) {
	if t == nil {
		log.Print("[WARN] Ignoring nil routing table")
		return
	}
	table.Store(t)
}

// Table contains a set of routes grouped by host.
// The host routes are sorted from most to least specific
// by sorting the routes in reverse order by path.
type Table map[string]Routes

// hostpath splits a 'host/path' prefix into 'host' and '/path' or it returns a
// ':port' prefix as ':port' and '' since there is no path component for TCP
// connections.
func hostpath(prefix string) (host string, path string) {
	if strings.HasPrefix(prefix, ":") {
		return prefix, ""
	}

	p := strings.SplitN(prefix, "/", 2)
	host, path = p[0], ""
	if len(p) == 1 {
		return p[0], "/"
	}
	return p[0], "/" + p[1]
}

func NewTable(s string) (t Table, err error) {
	defs, err := Parse(s)
	if err != nil {
		return nil, err
	}

	t = make(Table)
	for _, d := range defs {
		switch d.Cmd {
		case RouteAddCmd:
			err = t.addRoute(d)
		case RouteDelCmd:
			err = t.delRoute(d)
		case RouteWeightCmd:
			err = t.weighRoute(d)
		default:
			err = fmt.Errorf("route: invalid command: %s", d.Cmd)
		}
		if err != nil {
			return nil, err
		}
	}
	return t, nil
}

// addRoute adds a new route prefix -> target for the given service.
func (t Table) addRoute(d *RouteDef) error {
	host, path := hostpath(d.Src)

	if d.Src == "" {
		return errInvalidPrefix
	}

	if d.Dst == "" {
		return errInvalidTarget
	}

	targetURL, err := url.Parse(d.Dst)
	if err != nil {
		return fmt.Errorf("route: invalid target. %s", err)
	}

	switch {
	// add new host
	case t[host] == nil:
		g, err := glob.Compile(path)
		if err != nil {
			return err
		}
		r := &Route{Host: host, Path: path, Glob: g}
		r.addTarget(d.Service, targetURL, d.Weight, d.Tags, d.Opts)
		t[host] = Routes{r}

	// add new route to existing host
	case t[host].find(path) == nil:
		g, err := glob.Compile(path)
		if err != nil {
			return err
		}
		r := &Route{Host: host, Path: path, Glob: g}
		r.addTarget(d.Service, targetURL, d.Weight, d.Tags, d.Opts)
		t[host] = append(t[host], r)
		sort.Sort(t[host])

	// add new target to existing route
	default:
		t[host].find(path).addTarget(d.Service, targetURL, d.Weight, d.Tags, d.Opts)
	}

	return nil
}

func (t Table) weighRoute(d *RouteDef) error {
	host, path := hostpath(d.Src)

	if d.Src == "" {
		return errInvalidPrefix
	}

	if t[host] == nil || t[host].find(path) == nil {
		return errNoMatch
	}

	if n := t[host].find(path).setWeight(d.Service, d.Weight, d.Tags); n == 0 {
		return errNoMatch
	}
	return nil
}

// delRoute removes one or more routes depending on the arguments.
// If service, prefix and target are provided then only this route
// is removed. Are only service and prefix provided then all routes
// for this service and prefix are removed. This removes all active
// instances of the service from the route. If only the service is
// provided then all routes for this service are removed. The service
// will no longer receive traffic. Routes with no targets are removed.
func (t Table) delRoute(d *RouteDef) error {
	switch {
	case len(d.Tags) > 0:
		for _, routes := range t {
			for _, r := range routes {
				r.filter(func(tg *Target) bool {
					return (d.Service == "" || tg.Service == d.Service) && contains(tg.Tags, d.Tags)
				})
			}
		}

	case d.Src == "" && d.Dst == "":
		for _, routes := range t {
			for _, r := range routes {
				r.filter(func(tg *Target) bool {
					return tg.Service == d.Service
				})
			}
		}

	case d.Dst == "":
		r := t.route(hostpath(d.Src))
		if r == nil {
			return nil
		}
		r.filter(func(tg *Target) bool {
			return tg.Service == d.Service
		})

	default:
		targetURL, err := url.Parse(d.Dst)
		if err != nil {
			return fmt.Errorf("route: invalid target. %s", err)
		}

		r := t.route(hostpath(d.Src))
		if r == nil {
			return nil
		}
		r.filter(func(tg *Target) bool {
			return tg.Service == d.Service && tg.URL.String() == targetURL.String()
		})
	}

	// remove all routes without targets
	for host, routes := range t {
		var clone Routes
		for _, r := range routes {
			if len(r.Targets) == 0 {
				continue
			}
			clone = append(clone, r)
		}
		t[host] = clone
	}

	// remove all hosts without routes
	for host, routes := range t {
		if len(routes) == 0 {
			delete(t, host)
		}
	}

	return nil
}

// route finds the route for host/path or returns nil if none exists.
func (t Table) route(host, path string) *Route {
	routes := t[host]
	if routes == nil {
		return nil
	}
	return routes.find(path)
}

// normalizeHost returns the hostname from the request
// and removes the default port if present.
func normalizeHost(host string, tls bool) string {
	host = strings.ToLower(host)
	if !tls && strings.HasSuffix(host, ":80") {
		return host[:len(host)-len(":80")]
	}
	if tls && strings.HasSuffix(host, ":443") {
		return host[:len(host)-len(":443")]
	}
	return host
}

// matchingHosts returns all keys (host name patterns) from the
// routing table which match the normalized request hostname.
func (t Table) matchingHosts(req *http.Request) (hosts []string) {
	host := normalizeHost(req.Host, req.TLS != nil)
	for pattern := range t {
		normpat := normalizeHost(pattern, req.TLS != nil)
		g := glob.MustCompile(normpat)
		if g.Match(host) {
			hosts = append(hosts, pattern)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(hosts)))
	return hosts
}

// Lookup finds a target url based on the current matcher and picker
// or nil if there is none. It first checks the routes for the host
// and if none matches then it falls back to generic routes without
// a host. This is useful for a catch-all '/' rule.
func (t Table) Lookup(req *http.Request, trace string, pick picker, match matcher) (target *Target) {
	if trace != "" {
		if len(trace) > 16 {
			trace = trace[:15]
		}
		log.Printf("[TRACE] %s Tracing %s%s", trace, req.Host, req.URL.Path)
	}

	// find matching hosts for the request
	// and add "no host" as the fallback option
	hosts := t.matchingHosts(req)
	hosts = append(hosts, "")
	for _, h := range hosts {
		if target = t.lookup(h, req.URL.Path, trace, pick, match); target != nil {
			if target.RedirectCode != 0 {
				target.BuildRedirectURL(req.URL) // build redirect url and cache in target
				if target.RedirectURL.Scheme == req.Header.Get("X-Forwarded-Proto") &&
					target.RedirectURL.Host == req.Host &&
					target.RedirectURL.Path == req.URL.Path {
					log.Print("[INFO] Skipping redirect with same scheme, host and path")
					continue
				}
			}
			break
		}
	}

	if target != nil && trace != "" {
		log.Printf("[TRACE] %s Routing to service %s on %s", trace, target.Service, target.URL)
	}

	return target
}

func (t Table) LookupHost(host string, pick picker) *Target {
	return t.lookup(host, "/", "", pick, prefixMatcher)
}

func (t Table) lookup(host, path, trace string, pick picker, match matcher) *Target {
	for _, r := range t[host] {
		if match(path, r) {
			n := len(r.Targets)
			if n == 0 {
				return nil
			}

			var target *Target
			if n == 1 {
				target = r.Targets[0]
			} else {
				target = pick(r)
			}
			if trace != "" {
				log.Printf("[TRACE] %s Match %s%s", trace, r.Host, r.Path)
			}
			return target
		}
		if trace != "" {
			log.Printf("[TRACE] %s No match %s%s", trace, r.Host, r.Path)
		}
	}
	return nil
}

func (t Table) config(addWeight bool) []string {
	var hosts []string
	for host := range t {
		if host != "" {
			hosts = append(hosts, host)
		}
	}
	sort.Sort(sort.Reverse(sort.StringSlice(hosts)))

	// entries without host come last
	hosts = append(hosts, "")

	var cfg []string
	for _, host := range hosts {
		for _, routes := range t[host] {
			cfg = append(cfg, routes.config(addWeight)...)
		}
	}
	return cfg
}

// String returns the routing table as config file which can
// be read by Parse() again.
func (t Table) String() string {
	return strings.Join(t.config(false), "\n")
}

// Dump returns the routing table as a detailed
func (t Table) Dump() string {
	w := new(bytes.Buffer)

	hosts := []string{}
	for k := range t {
		hosts = append(hosts, k)
	}
	sort.Strings(hosts)

	last := func(n, total int) bool {
		return n == total-1
	}

	for i, h := range hosts {
		fmt.Fprintf(w, "+-- host=%s\n", h)

		routes := t[h]
		for j, r := range routes {
			p0 := "|   "
			if last(i, len(hosts)) {
				p0 = "    "
			}
			p1 := "|-- "
			if last(j, len(routes)) {
				p1 = "+-- "
			}

			fmt.Fprintf(w, "%s%spath=%s\n", p0, p1, r.Path)

			m := map[*Target]int{}
			for _, t := range r.wTargets {
				m[t] += 1
			}

			total := len(r.wTargets)
			k := 0
			for t, n := range m {
				p1 := "|    "
				if last(j, len(routes)) {
					p1 = "    "
				}
				p2 := "|-- "
				if last(k, len(m)) {
					p2 = "+-- "
				}
				weight := float64(n) / float64(total)
				fmt.Fprintf(w, "%s%s%saddr=%s weight %2.2f slots %d/%d\n", p0, p1, p2, t.URL.Host, weight, n, total)
				k++
			}
		}
	}
	return w.String()
}
