// Copyright © 2018 Heptio
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package dag provides a data model, in the form of a directed acyclic graph,
// of the relationship between Kubernetes Ingress, Service, and Secret objects.
package dag

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	v1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	"k8s.io/apimachinery/pkg/util/intstr"

	"github.com/envoyproxy/go-control-plane/envoy/api/v2/auth"
	ingressroutev1 "github.com/heptio/contour/apis/contour/v1beta1"
)

const (
	StatusValid    = "valid"
	StatusInvalid  = "invalid"
	StatusOrphaned = "orphaned"
)

// BuildDAG returns a new DAG from the supplied KubernetesCache.
func BuildDAG(kc *KubernetesCache) *DAG {
	builder := &builder{source: kc}
	return builder.compute()
}

// A builder holds the state of one invocation of Builder.Build.
// Once used, the builder should be discarded.
type builder struct {
	source *KubernetesCache

	services  map[servicemeta]Service
	secrets   map[meta]*Secret
	listeners map[int]*Listener

	orphaned map[meta]bool

	statuses []Status
}

// lookupHTTPService returns a HTTPService that matches the meta and port supplied.
func (b *builder) lookupHTTPService(m meta, port intstr.IntOrString) *HTTPService {
	s := b.lookupService(m, port)
	switch s := s.(type) {
	case *HTTPService:
		return s
	case nil:
		svc, ok := b.source.services[m]
		if !ok {
			return nil
		}
		for i := range svc.Spec.Ports {
			p := &svc.Spec.Ports[i]
			if int(p.Port) == port.IntValue() {
				return b.addHTTPService(svc, p)
			}
			if port.String() == p.Name {
				return b.addHTTPService(svc, p)
			}
		}
		return nil
	default:
		// some other type
		return nil
	}
}

// lookupTCPService returns a TCPService that matches the meta and port supplied.
func (b *builder) lookupTCPService(m meta, port intstr.IntOrString) *TCPService {
	s := b.lookupService(m, port)
	switch s := s.(type) {
	case *TCPService:
		return s
	case nil:
		svc, ok := b.source.services[m]
		if !ok {
			return nil
		}
		for i := range svc.Spec.Ports {
			p := &svc.Spec.Ports[i]
			if int(p.Port) == port.IntValue() {
				return b.addTCPService(svc, p)
			}
			if port.String() == p.Name {
				return b.addTCPService(svc, p)
			}
		}
		return nil
	default:
		// some other type
		return nil
	}
}
func (b *builder) lookupService(m meta, port intstr.IntOrString) Service {
	if port.Type != intstr.Int {
		// can't handle, give up
		return nil
	}
	sm := servicemeta{
		name:      m.name,
		namespace: m.namespace,
		port:      int32(port.IntValue()),
	}
	s, ok := b.services[sm]
	if !ok {
		return nil // avoid typed nil
	}
	return s
}

func (b *builder) addHTTPService(svc *v1.Service, port *v1.ServicePort) *HTTPService {
	if b.services == nil {
		b.services = make(map[servicemeta]Service)
	}
	up := parseUpstreamProtocols(svc.Annotations, annotationUpstreamProtocol, "h2", "h2c", "tls")
	protocol := up[port.Name]
	if protocol == "" {
		protocol = up[strconv.Itoa(int(port.Port))]
	}

	s := &HTTPService{
		TCPService: TCPService{
			Name:        svc.Name,
			Namespace:   svc.Namespace,
			ServicePort: port,

			MaxConnections:     parseAnnotation(svc.Annotations, annotationMaxConnections),
			MaxPendingRequests: parseAnnotation(svc.Annotations, annotationMaxPendingRequests),
			MaxRequests:        parseAnnotation(svc.Annotations, annotationMaxRequests),
			MaxRetries:         parseAnnotation(svc.Annotations, annotationMaxRetries),
			ExternalName:       externalName(svc),
		},
		Protocol: protocol,
	}
	b.services[s.toMeta()] = s
	return s
}

func externalName(svc *v1.Service) string {
	if svc.Spec.Type != v1.ServiceTypeExternalName {
		return ""
	}
	return svc.Spec.ExternalName
}

func (b *builder) addTCPService(svc *v1.Service, port *v1.ServicePort) *TCPService {
	if b.services == nil {
		b.services = make(map[servicemeta]Service)
	}
	s := &TCPService{
		Name:        svc.Name,
		Namespace:   svc.Namespace,
		ServicePort: port,

		MaxConnections:     parseAnnotation(svc.Annotations, annotationMaxConnections),
		MaxPendingRequests: parseAnnotation(svc.Annotations, annotationMaxPendingRequests),
		MaxRequests:        parseAnnotation(svc.Annotations, annotationMaxRequests),
		MaxRetries:         parseAnnotation(svc.Annotations, annotationMaxRetries),
	}
	b.services[s.toMeta()] = s
	return s
}

// lookupSecret returns a Secret if present or nil if the underlying kubernetes
// secret fails validation or is missing.
func (b *builder) lookupSecret(m meta, validate func(*v1.Secret) bool) *Secret {
	if s, ok := b.secrets[m]; ok {
		return s
	}
	sec, ok := b.source.secrets[m]
	if !ok {
		return nil
	}
	if !validate(sec) {
		return nil
	}
	s := &Secret{
		Object: sec,
	}
	if b.secrets == nil {
		b.secrets = make(map[meta]*Secret)
	}
	b.secrets[s.toMeta()] = s
	return s
}

func (b *builder) lookupVirtualHost(name string) *VirtualHost {
	l := b.listener(80)
	vh, ok := l.VirtualHosts[name]
	if !ok {
		vh := &VirtualHost{
			Name: name,
		}
		l.VirtualHosts[vh.Name] = vh
		return vh
	}
	return vh.(*VirtualHost)
}

func (b *builder) lookupSecureVirtualHost(name string) *SecureVirtualHost {
	l := b.listener(443)
	svh, ok := l.VirtualHosts[name]
	if !ok {
		svh := &SecureVirtualHost{
			VirtualHost: VirtualHost{
				Name: name,
			},
		}
		l.VirtualHosts[svh.VirtualHost.Name] = svh
		return svh
	}
	return svh.(*SecureVirtualHost)
}

// listener returns a listener for the supplied port.
// TODO: the port value is not actually used as a port
// anywhere. It's only used to choose between
// 80 (for insecure) or 443 (for secure). This should be
// fixed, see https://github.com/heptio/contour/issues/1135
func (b *builder) listener(port int) *Listener {
	l, ok := b.listeners[port]
	if !ok {
		l = &Listener{
			Port:         port,
			VirtualHosts: make(map[string]Vertex),
		}
		if b.listeners == nil {
			b.listeners = make(map[int]*Listener)
		}
		b.listeners[l.Port] = l
	}
	return l
}

func (b *builder) compute() *DAG {
	b.source.mu.RLock() // blocks mutation of the underlying cache until compute is done.
	defer b.source.mu.RUnlock()

	// setup secure vhosts if there is a matching secret
	// we do this first so that the set of active secure vhosts is stable
	// during computeIngresses.
	b.computeSecureVirtualhosts()

	b.computeIngresses()

	b.computeIngressRoutes()

	return b.DAG()
}

// prefixRoute returns a new dag.Route for the (ingress,prefix) tuple.
func prefixRoute(ingress *v1beta1.Ingress, prefix string) *Route {
	// compute websocket enabled routes
	wr := websocketRoutes(ingress)

	var retry *RetryPolicy
	if retryOn, ok := ingress.Annotations[annotationRetryOn]; ok && len(retryOn) > 0 {
		// if there is a non empty retry-on annotation, build a RetryPolicy manually.
		retry = &RetryPolicy{
			RetryOn: retryOn,
			// TODO(dfc) NumRetries may parse as 0, which is inconsistent with
			// retryPolicy()'s default value of 1.
			NumRetries: parseAnnotation(ingress.Annotations, annotationNumRetries),
			// TODO(dfc) PerTryTimeout will parse to -1, infinite, in the case of
			// invalid data, this is inconsistent with retryPolicy()'s default value
			// of 0 duration.
			PerTryTimeout: parseTimeout(ingress.Annotations[annotationPerTryTimeout]),
		}
	}

	var timeout *TimeoutPolicy
	if request, ok := ingress.Annotations[annotationRequestTimeout]; ok {
		// if the request timeout annotation is present on this ingress
		// construct and use the ingressroute timeout policy logic.
		timeout = timeoutPolicy(&ingressroutev1.TimeoutPolicy{
			Request: request,
		})
	}

	return &Route{
		Prefix:        prefix,
		HTTPSUpgrade:  tlsRequired(ingress),
		Websocket:     wr[prefix],
		TimeoutPolicy: timeout,
		RetryPolicy:   retry,
	}
}

// isBlank indicates if a string contains nothing but blank characters.
func isBlank(s string) bool {
	return len(strings.TrimSpace(s)) == 0
}

// minProtoVersion returns the TLS protocol version specified by an ingress annotation
// or default if non present.
func minProtoVersion(version string) auth.TlsParameters_TlsProtocol {
	switch version {
	case "1.3":
		return auth.TlsParameters_TLSv1_3
	case "1.2":
		return auth.TlsParameters_TLSv1_2
	default:
		// any other value is interpreted as TLS/1.1
		return auth.TlsParameters_TLSv1_1
	}
}

// validIngressRoutes returns a slice of *ingressroutev1.IngressRoute objects.
// invalid IngressRoute objects are excluded from the slice and a corresponding entry
// added via setStatus.
func (b *builder) validIngressRoutes() []*ingressroutev1.IngressRoute {
	// ensure that a given fqdn is only referenced in a single ingressroute resource
	var valid []*ingressroutev1.IngressRoute
	fqdnIngressroutes := make(map[string][]*ingressroutev1.IngressRoute)
	for _, ir := range b.source.ingressroutes {
		if ir.Spec.VirtualHost == nil {
			valid = append(valid, ir)
			continue
		}
		fqdnIngressroutes[ir.Spec.VirtualHost.Fqdn] = append(fqdnIngressroutes[ir.Spec.VirtualHost.Fqdn], ir)
	}

	for fqdn, irs := range fqdnIngressroutes {
		switch len(irs) {
		case 1:
			valid = append(valid, irs[0])
		default:
			// multiple irs use the same fqdn. mark them as invalid.
			var conflicting []string
			for _, ir := range irs {
				conflicting = append(conflicting, fmt.Sprintf("%s/%s", ir.Namespace, ir.Name))
			}
			sort.Strings(conflicting) // sort for test stability
			msg := fmt.Sprintf("fqdn %q is used in multiple IngressRoutes: %s", fqdn, strings.Join(conflicting, ", "))
			for _, ir := range irs {
				b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: msg, Vhost: fqdn})
			}
		}
	}
	return valid
}

// computeSecureVirtualhosts populates tls parameters of
// secure virtual hosts.
func (b *builder) computeSecureVirtualhosts() {
	for _, ing := range b.source.ingresses {
		for _, tls := range ing.Spec.TLS {
			m := splitSecret(tls.SecretName, ing.Namespace)
			if sec := b.lookupSecret(m, validSecret); sec != nil && b.delegationPermitted(m, ing.Namespace) {
				for _, host := range tls.Hosts {
					svhost := b.lookupSecureVirtualHost(host)
					svhost.Secret = sec
					version := ing.Annotations["contour.heptio.com/tls-minimum-protocol-version"]
					svhost.MinProtoVersion = minProtoVersion(version)
				}
			}
		}
	}
}

// splitSecret splits a secretName into its namespace and name components.
// If there is no namespace prefix, the default namespace is returned.
func splitSecret(secret, defns string) meta {
	v := strings.SplitN(secret, "/", 2)
	switch len(v) {
	case 1:
		// no prefix
		return meta{
			name:      v[0],
			namespace: defns,
		}
	default:
		return meta{
			name:      v[1],
			namespace: stringOrDefault(v[0], defns),
		}
	}
}

func (b *builder) delegationPermitted(secret meta, to string) bool {
	contains := func(haystack []string, needle string) bool {
		if len(haystack) == 1 && haystack[0] == "*" {
			return true
		}
		for _, h := range haystack {
			if h == needle {
				return true
			}
		}
		return false
	}

	if secret.namespace == to {
		// secret is in the same namespace as target
		return true
	}
	for _, d := range b.source.delegations {
		if d.Namespace != secret.namespace {
			continue
		}
		for _, d := range d.Spec.Delegations {
			if contains(d.TargetNamespaces, to) {
				if secret.name == d.SecretName {
					return true
				}
			}
		}
	}
	return false
}

func (b *builder) computeIngresses() {
	// deconstruct each ingress into routes and virtualhost entries
	for _, ing := range b.source.ingresses {

		// rewrite the default ingress to a stock ingress rule.
		rules := rulesFromSpec(ing.Spec)

		for _, rule := range rules {
			host := stringOrDefault(rule.Host, "*")
			for _, httppath := range httppaths(rule) {
				prefix := stringOrDefault(httppath.Path, "/")
				r := prefixRoute(ing, prefix)
				be := httppath.Backend
				m := meta{name: be.ServiceName, namespace: ing.Namespace}
				if s := b.lookupHTTPService(m, be.ServicePort); s != nil {
					r.Clusters = append(r.Clusters, &Cluster{Upstream: s})
				}

				// should we create port 80 routes for this ingress
				if tlsRequired(ing) || httpAllowed(ing) {
					b.lookupVirtualHost(host).addRoute(r)
				}

				if b.secureVirtualhostExists(host) && host != "*" {
					b.lookupSecureVirtualHost(host).addRoute(r)
				}
			}
		}
	}
}

func stringOrDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (b *builder) computeIngressRoutes() {
	for _, ir := range b.validIngressRoutes() {
		if ir.Spec.VirtualHost == nil {
			// mark delegate ingressroute orphaned.
			b.setOrphaned(ir)
			continue
		}

		// ensure root ingressroute lives in allowed namespace
		if !b.rootAllowed(ir) {
			b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: "root IngressRoute cannot be defined in this namespace"})
			continue
		}

		host := ir.Spec.VirtualHost.Fqdn
		if isBlank(host) {
			b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: "Spec.VirtualHost.Fqdn must be specified"})
			continue
		}

		var enforceTLS, passthrough bool
		if tls := ir.Spec.VirtualHost.TLS; tls != nil {
			// attach secrets to TLS enabled vhosts
			m := splitSecret(tls.SecretName, ir.Namespace)
			sec := b.lookupSecret(m, validSecret)
			secretInvalidOrNotFound := sec == nil
			if sec != nil && b.delegationPermitted(m, ir.Namespace) {
				svhost := b.lookupSecureVirtualHost(host)
				svhost.Secret = sec
				svhost.MinProtoVersion = minProtoVersion(ir.Spec.VirtualHost.TLS.MinimumProtocolVersion)
				enforceTLS = true
			}
			// passthrough is true if tls.secretName is not present, and
			// tls.passthrough is set to true.
			passthrough = isBlank(tls.SecretName) && tls.Passthrough

			// If not passthrough and secret is invalid, then set status
			if secretInvalidOrNotFound && !passthrough {
				b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: "TLS Secret not found or is malformed"})
			}
		}

		switch {
		case ir.Spec.TCPProxy != nil && (passthrough || enforceTLS):
			b.processTCPProxy(ir, nil, host)
		case ir.Spec.Routes != nil:
			b.processRoutes(ir, "", nil, host, enforceTLS)
		}
	}
}

func (b *builder) secureVirtualhostExists(host string) bool {
	_, ok := b.listener(443).VirtualHosts[host]
	return ok
}

// rulesFromSpec merges the IngressSpec's Rules with a synthetic
// rule representing the default backend.
func rulesFromSpec(spec v1beta1.IngressSpec) []v1beta1.IngressRule {
	rules := spec.Rules
	if backend := spec.Backend; backend != nil {
		rule := defaultBackendRule(backend)
		rules = append(rules, rule)
	}
	return rules
}

// defaultBackendRule returns an IngressRule that represents the IngressBackend.
func defaultBackendRule(be *v1beta1.IngressBackend) v1beta1.IngressRule {
	return v1beta1.IngressRule{
		IngressRuleValue: v1beta1.IngressRuleValue{
			HTTP: &v1beta1.HTTPIngressRuleValue{
				Paths: []v1beta1.HTTPIngressPath{{
					Backend: v1beta1.IngressBackend{
						ServiceName: be.ServiceName,
						ServicePort: be.ServicePort,
					},
				}},
			},
		},
	}
}

// DAG returns a *DAG representing the current state of this builder.
func (b *builder) DAG() *DAG {
	var dag DAG
	for _, l := range b.listeners {
		for k, vh := range l.VirtualHosts {
			switch vh := vh.(type) {
			case *VirtualHost:
				// suppress virtual hosts without routes.
				if len(vh.routes) < 1 {
					delete(l.VirtualHosts, k)
				}
			case *SecureVirtualHost:
				// suppress secure virtual hosts without secrets or tcpproxy.
				if vh.Secret == nil && vh.TCPProxy == nil {
					delete(l.VirtualHosts, k)
				}
			}
		}
		// suppress empty listeners
		if len(l.VirtualHosts) > 0 {
			dag.roots = append(dag.roots, l)
		}
	}
	for meta := range b.orphaned {
		ir, ok := b.source.ingressroutes[meta]
		if ok {
			b.setStatus(Status{Object: ir, Status: StatusOrphaned, Description: "this IngressRoute is not part of a delegation chain from a root IngressRoute"})
		}
	}
	dag.statuses = b.statuses
	return &dag
}

// setStatus assigns a status to an object.
func (b *builder) setStatus(st Status) {
	b.statuses = append(b.statuses, st)
}

// setOrphaned records an ingressroute as orphaned.
func (b *builder) setOrphaned(ir *ingressroutev1.IngressRoute) {
	if b.orphaned == nil {
		b.orphaned = make(map[meta]bool)
	}
	m := meta{name: ir.Name, namespace: ir.Namespace}
	b.orphaned[m] = true
}

// rootAllowed returns true if the ingressroute lives in a permitted root namespace.
func (b *builder) rootAllowed(ir *ingressroutev1.IngressRoute) bool {
	if len(b.source.IngressRouteRootNamespaces) == 0 {
		return true
	}
	for _, ns := range b.source.IngressRouteRootNamespaces {
		if ns == ir.Namespace {
			return true
		}
	}
	return false
}

// validSecret returns true if the Secret contains certificate and private key material.
func validSecret(s *v1.Secret) bool {
	return s.Type == v1.SecretTypeTLS && len(s.Data[v1.TLSCertKey]) > 0 && len(s.Data[v1.TLSPrivateKeyKey]) > 0
}

func validCA(s *v1.Secret) bool {
	return len(s.Data["ca.crt"]) > 0
}

func (b *builder) processRoutes(ir *ingressroutev1.IngressRoute, prefixMatch string, visited []*ingressroutev1.IngressRoute, host string, enforceTLS bool) {
	visited = append(visited, ir)

	for _, route := range ir.Spec.Routes {
		// route cannot both delegate and point to services
		if len(route.Services) > 0 && route.Delegate != nil {
			b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: fmt.Sprintf("route %q: cannot specify services and delegate in the same route", route.Match), Vhost: host})
			return
		}

		// base case: The route points to services, so we add them to the vhost
		if len(route.Services) > 0 {
			if !matchesPathPrefix(route.Match, prefixMatch) {
				b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: fmt.Sprintf("the path prefix %q does not match the parent's path prefix %q", route.Match, prefixMatch), Vhost: host})
				return
			}

			r := &Route{
				Prefix:                 route.Match,
				Websocket:              route.EnableWebsockets,
				HTTPSUpgrade:           routeEnforceTLS(enforceTLS, route.PermitInsecure),
				PrefixRewrite:          route.PrefixRewrite,
				TimeoutPolicy:          timeoutPolicy(route.TimeoutPolicy),
				RetryPolicy:            retryPolicy(route.RetryPolicy),
				RateLimitConfiguration: route.RateLimitConfiguration,
			}
			for _, service := range route.Services {
				if service.Port < 1 || service.Port > 65535 {
					b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: fmt.Sprintf("route %q: service %q: port must be in the range 1-65535", route.Match, service.Name), Vhost: host})
					return
				}
				if service.Weight < 0 {
					b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: fmt.Sprintf("route %q: service %q: weight must be greater than or equal to zero", route.Match, service.Name), Vhost: host})
					return
				}
				m := meta{name: service.Name, namespace: ir.Namespace}
				if s := b.lookupHTTPService(m, intstr.FromInt(service.Port)); s != nil {
					var uv *UpstreamValidation
					if s.Protocol == "tls" {
						// we can only varlidate TLS connections to services that talk TLS
						uv = b.lookupUpstreamValidation(ir, host, route, service, ir.Namespace)
					}
					r.Clusters = append(r.Clusters, &Cluster{
						Upstream:             s,
						LoadBalancerStrategy: service.Strategy,
						Weight:               service.Weight,
						HealthCheck:          service.HealthCheck,
						UpstreamValidation:   uv,
					})
				}
			}

			b.lookupVirtualHost(host).addRoute(r)
			b.lookupSecureVirtualHost(host).addRoute(r)
			continue
		}

		if route.Delegate == nil {
			// not a delegate route
			continue
		}

		namespace := route.Delegate.Namespace
		if namespace == "" {
			// we are delegating to another IngressRoute in the same namespace
			namespace = ir.Namespace
		}

		if dest, ok := b.source.ingressroutes[meta{name: route.Delegate.Name, namespace: namespace}]; ok {
			// dest is not an orphaned ingress route, as there is an IR that points to it
			delete(b.orphaned, meta{name: dest.Name, namespace: dest.Namespace})

			// ensure we are not following an edge that produces a cycle
			var path []string
			for _, vir := range visited {
				path = append(path, fmt.Sprintf("%s/%s", vir.Namespace, vir.Name))
			}
			for _, vir := range visited {
				if dest.Name == vir.Name && dest.Namespace == vir.Namespace {
					path = append(path, fmt.Sprintf("%s/%s", dest.Namespace, dest.Name))
					description := fmt.Sprintf("route creates a delegation cycle: %s", strings.Join(path, " -> "))
					b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: description, Vhost: host})
					return
				}
			}

			// follow the link and process the target ingress route
			b.processRoutes(dest, route.Match, visited, host, enforceTLS)
		}
	}

	b.setStatus(Status{Object: ir, Status: StatusValid, Description: "valid IngressRoute", Vhost: host})
}

// TODO(dfc) needs unit tests; we should pass in some kind of context object that encasulates all the properties we need for reporting
// status here, the ir, the host, the route, etc. I'm thinking something like logrus' WithField.

func (b *builder) lookupUpstreamValidation(ir *ingressroutev1.IngressRoute, host string, route ingressroutev1.Route, service ingressroutev1.Service, namespace string) *UpstreamValidation {
	uv := service.UpstreamValidation
	if uv == nil {
		// no upstream validation requested, nothing to do
		return nil
	}

	cacert := b.lookupSecret(meta{name: uv.CACertificate, namespace: namespace}, validCA)
	if cacert == nil {
		// UpstreamValidation is requested, but cert is missing or not configured
		b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: fmt.Sprintf("route %q: service %q: upstreamValidation requested but secret not found or misconfigured", route.Match, service.Name), Vhost: host})
		return nil
	}

	if uv.SubjectName == "" {
		// UpstreamValidation is requested, but SAN is not provided
		b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: fmt.Sprintf("route %q: service %q: upstreamValidation requested but subject alt name not found or misconfigured", route.Match, service.Name), Vhost: host})
		return nil
	}

	return &UpstreamValidation{
		CACertificate: cacert,
		SubjectName:   uv.SubjectName,
	}
}

func (b *builder) processTCPProxy(ir *ingressroutev1.IngressRoute, visited []*ingressroutev1.IngressRoute, host string) {
	visited = append(visited, ir)

	// tcpproxy cannot both delegate and point to services
	tcpproxy := ir.Spec.TCPProxy
	if len(tcpproxy.Services) > 0 && tcpproxy.Delegate != nil {
		b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: "tcpproxy: cannot specify services and delegate in the same tcpproxy", Vhost: host})
		return
	}

	if len(tcpproxy.Services) > 0 {
		var proxy TCPProxy
		for _, service := range tcpproxy.Services {
			m := meta{name: service.Name, namespace: ir.Namespace}
			s := b.lookupTCPService(m, intstr.FromInt(service.Port))
			if s == nil {
				b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: fmt.Sprintf("tcpproxy: service %s/%s/%d: not found", ir.Namespace, service.Name, service.Port), Vhost: host})
				return
			}
			proxy.Clusters = append(proxy.Clusters, &Cluster{
				Upstream:             s,
				LoadBalancerStrategy: service.Strategy,
			})
		}
		b.lookupSecureVirtualHost(host).VirtualHost.TCPProxy = &proxy
		b.setStatus(Status{Object: ir, Status: StatusValid, Description: "valid IngressRoute", Vhost: host})
		return
	}

	if tcpproxy.Delegate == nil {
		// not a delegate tcpproxy
		return
	}

	namespace := tcpproxy.Delegate.Namespace
	if namespace == "" {
		// we are delegating to another IngressRoute in the same namespace
		namespace = ir.Namespace
	}

	if dest, ok := b.source.ingressroutes[meta{name: tcpproxy.Delegate.Name, namespace: namespace}]; ok {
		// dest is not an orphaned ingress route, as there is an IR that points to it
		delete(b.orphaned, meta{name: dest.Name, namespace: dest.Namespace})

		// ensure we are not following an edge that produces a cycle
		var path []string
		for _, vir := range visited {
			path = append(path, fmt.Sprintf("%s/%s", vir.Namespace, vir.Name))
		}
		for _, vir := range visited {
			if dest.Name == vir.Name && dest.Namespace == vir.Namespace {
				path = append(path, fmt.Sprintf("%s/%s", dest.Namespace, dest.Name))
				description := fmt.Sprintf("tcpproxy creates a delegation cycle: %s", strings.Join(path, " -> "))
				b.setStatus(Status{Object: ir, Status: StatusInvalid, Description: description, Vhost: host})
				return
			}
		}

		// follow the link and process the target ingress route
		b.processTCPProxy(dest, visited, host)
	}

	b.setStatus(Status{Object: ir, Status: StatusValid, Description: "valid IngressRoute", Vhost: host})
}

// routeEnforceTLS determines if the route should redirect the user to a secure TLS listener
func routeEnforceTLS(enforceTLS, permitInsecure bool) bool {
	return enforceTLS && !permitInsecure
}

// httppaths returns a slice of HTTPIngressPath values for a given IngressRule.
// In the case that the IngressRule contains no valid HTTPIngressPaths, a
// nil slice is returned.
func httppaths(rule v1beta1.IngressRule) []v1beta1.HTTPIngressPath {
	if rule.IngressRuleValue.HTTP == nil {
		// rule.IngressRuleValue.HTTP value is optional.
		return nil
	}
	return rule.IngressRuleValue.HTTP.Paths
}

// matchesPathPrefix checks whether the given path matches the given prefix
func matchesPathPrefix(path, prefix string) bool {
	if len(prefix) == 0 {
		return true
	}
	// an empty string cannot have a prefix
	if len(path) == 0 {
		return false
	}
	if prefix[len(prefix)-1] != '/' {
		prefix += "/"
	}
	if path[len(path)-1] != '/' {
		path += "/"
	}
	return strings.HasPrefix(path, prefix)
}

// Status contains the status for an IngressRoute (valid / invalid / orphan, etc)
type Status struct {
	Object      *ingressroutev1.IngressRoute
	Status      string
	Description string
	Vhost       string
}
