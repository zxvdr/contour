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

package contour

import (
	"testing"

	v2 "github.com/envoyproxy/go-control-plane/envoy/api/v2"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/auth"
	"github.com/envoyproxy/go-control-plane/envoy/api/v2/listener"
	"github.com/gogo/protobuf/proto"
	"github.com/google/go-cmp/cmp"
	ingressroutev1 "github.com/heptio/contour/apis/contour/v1beta1"
	"github.com/heptio/contour/internal/dag"
	"github.com/heptio/contour/internal/envoy"
	"github.com/heptio/contour/internal/metrics"
	"github.com/prometheus/client_golang/prometheus"
	v1 "k8s.io/api/core/v1"
	"k8s.io/api/extensions/v1beta1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestListenerCacheContents(t *testing.T) {
	tests := map[string]struct {
		contents map[string]*v2.Listener
		want     []proto.Message
	}{
		"empty": {
			contents: nil,
			want:     nil,
		},
		"simple": {
			contents: listenermap(&v2.Listener{
				Name:         ENVOY_HTTP_LISTENER,
				Address:      *envoy.SocketAddress("0.0.0.0", 8080),
				FilterChains: defaultfilterchain(),
			}),
			want: []proto.Message{
				&v2.Listener{
					Name:         ENVOY_HTTP_LISTENER,
					Address:      *envoy.SocketAddress("0.0.0.0", 8080),
					FilterChains: defaultfilterchain(),
				},
			},
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var lc ListenerCache
			lc.Update(tc.contents)
			got := lc.Contents()
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestListenerCacheQuery(t *testing.T) {
	tests := map[string]struct {
		contents map[string]*v2.Listener
		query    []string
		want     []proto.Message
	}{
		"exact match": {
			contents: listenermap(&v2.Listener{
				Name:         ENVOY_HTTP_LISTENER,
				Address:      *envoy.SocketAddress("0.0.0.0", 8080),
				FilterChains: defaultfilterchain(),
			}),
			query: []string{ENVOY_HTTP_LISTENER},
			want: []proto.Message{
				&v2.Listener{
					Name:         ENVOY_HTTP_LISTENER,
					Address:      *envoy.SocketAddress("0.0.0.0", 8080),
					FilterChains: defaultfilterchain(),
				},
			},
		},
		"partial match": {
			contents: listenermap(&v2.Listener{
				Name:         ENVOY_HTTP_LISTENER,
				Address:      *envoy.SocketAddress("0.0.0.0", 8080),
				FilterChains: defaultfilterchain(),
			}),
			query: []string{ENVOY_HTTP_LISTENER, "stats-listener"},
			want: []proto.Message{
				&v2.Listener{
					Name:         ENVOY_HTTP_LISTENER,
					Address:      *envoy.SocketAddress("0.0.0.0", 8080),
					FilterChains: defaultfilterchain(),
				},
			},
		},
		"no match": {
			contents: listenermap(&v2.Listener{
				Name:         ENVOY_HTTP_LISTENER,
				Address:      *envoy.SocketAddress("0.0.0.0", 8080),
				FilterChains: defaultfilterchain(),
			}),
			query: []string{"stats-listener"},
			want:  nil,
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			var lc ListenerCache
			lc.Update(tc.contents)
			got := lc.Query(tc.query)
			if diff := cmp.Diff(tc.want, got); diff != "" {
				t.Fatal(diff)
			}
		})
	}
}

func TestListenerVisit(t *testing.T) {
	tests := map[string]struct {
		ListenerVisitorConfig
		objs []interface{}
		want map[string]*v2.Listener
	}{
		"nothing": {
			objs: nil,
			want: map[string]*v2.Listener{},
		},
		"one http only ingress": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
			},
			want: listenermap(&v2.Listener{
				Name:         ENVOY_HTTP_LISTENER,
				Address:      *envoy.SocketAddress("0.0.0.0", 8080),
				FilterChains: defaultfilterchain(),
			}),
		},
		"one http only ingressroute": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{
							Fqdn: "www.example.com",
						},
						Routes: []ingressroutev1.Route{
							{
								Services: []ingressroutev1.Service{
									{
										Name: "backend",
										Port: 80,
									},
								},
							},
						},
					},
				},
			},
			want: listenermap(&v2.Listener{
				Name:         ENVOY_HTTP_LISTENER,
				Address:      *envoy.SocketAddress("0.0.0.0", 8080),
				FilterChains: defaultfilterchain(),
			}),
		},
		"simple ingress with secret": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						TLS: []v1beta1.IngressTLS{{
							Hosts:      []string{"whatever.example.com"},
							SecretName: "secret",
						}},
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
			},
			want: listenermap(&v2.Listener{
				Name:         ENVOY_HTTP_LISTENER,
				Address:      *envoy.SocketAddress("0.0.0.0", 8080),
				FilterChains: defaultfilterchain(),
			}, &v2.Listener{
				Name:    ENVOY_HTTPS_LISTENER,
				Address: *envoy.SocketAddress("0.0.0.0", 8443),
				ListenerFilters: []listener.ListenerFilter{
					envoy.TLSInspector(),
				},
				FilterChains: []listener.FilterChain{{
					FilterChainMatch: &listener.FilterChainMatch{
						ServerNames: []string{"whatever.example.com"},
					},
					TlsContext: tlscontext(auth.TlsParameters_TLSv1_1, "h2", "http/1.1"),
					Filters:    defaultfilters(),
				}},
			}),
		},
		"multiple tls ingress with secrets should be sorted": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sortedsecond",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						TLS: []v1beta1.IngressTLS{{
							Hosts:      []string{"sortedsecond.example.com"},
							SecretName: "secret",
						}},
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "sortedfirst",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						TLS: []v1beta1.IngressTLS{{
							Hosts:      []string{"sortedfirst.example.com"},
							SecretName: "secret",
						}},
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
			},
			want: listenermap(&v2.Listener{
				Name:         ENVOY_HTTP_LISTENER,
				Address:      *envoy.SocketAddress("0.0.0.0", 8080),
				FilterChains: defaultfilterchain(),
			}, &v2.Listener{
				Name:    ENVOY_HTTPS_LISTENER,
				Address: *envoy.SocketAddress("0.0.0.0", 8443),
				ListenerFilters: []listener.ListenerFilter{
					envoy.TLSInspector(),
				},
				FilterChains: []listener.FilterChain{
					{
						FilterChainMatch: &listener.FilterChainMatch{
							ServerNames: []string{"sortedfirst.example.com"},
						},
						TlsContext: tlscontext(auth.TlsParameters_TLSv1_1, "h2", "http/1.1"),
						Filters:    defaultfilters(),
					},
					{
						FilterChainMatch: &listener.FilterChainMatch{
							ServerNames: []string{"sortedsecond.example.com"},
						},
						TlsContext: tlscontext(auth.TlsParameters_TLSv1_1, "h2", "http/1.1"),
						Filters:    defaultfilters(),
					},
				},
			}),
		},
		"simple ingress with missing secret": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						TLS: []v1beta1.IngressTLS{{
							Hosts:      []string{"whatever.example.com"},
							SecretName: "missing",
						}},
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
			},
			want: listenermap(&v2.Listener{
				Name:         ENVOY_HTTP_LISTENER,
				Address:      *envoy.SocketAddress("0.0.0.0", 8080),
				FilterChains: defaultfilterchain(),
			}),
		},
		"simple ingressroute with secret": {
			objs: []interface{}{
				&ingressroutev1.IngressRoute{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: ingressroutev1.IngressRouteSpec{
						VirtualHost: &ingressroutev1.VirtualHost{
							Fqdn: "www.example.com",
							TLS: &ingressroutev1.TLS{
								SecretName: "secret",
							},
						},
						Routes: []ingressroutev1.Route{
							{
								Services: []ingressroutev1.Service{
									{
										Name: "backend",
										Port: 80,
									},
								},
							},
						},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
			},
			want: listenermap(&v2.Listener{
				Name:         ENVOY_HTTP_LISTENER,
				Address:      *envoy.SocketAddress("0.0.0.0", 8080),
				FilterChains: defaultfilterchain(),
			}, &v2.Listener{
				Name:    ENVOY_HTTPS_LISTENER,
				Address: *envoy.SocketAddress("0.0.0.0", 8443),
				FilterChains: []listener.FilterChain{{
					FilterChainMatch: &listener.FilterChainMatch{
						ServerNames: []string{"www.example.com"},
					},
					TlsContext: tlscontext(auth.TlsParameters_TLSv1_1, "h2", "http/1.1"),
					Filters:    defaultfilters(),
				}},
				ListenerFilters: []listener.ListenerFilter{
					envoy.TLSInspector(),
				},
			}),
		},
		"ingress with allow-http: false": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "kuard",
						Namespace: "default",
						Annotations: map[string]string{
							"kubernetes.io/ingress.allow-http": "false",
						},
					},
					Spec: v1beta1.IngressSpec{
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
			},
			want: map[string]*v2.Listener{},
		},
		"simple tls ingress with allow-http:false": {
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
						Annotations: map[string]string{
							"kubernetes.io/ingress.allow-http": "false",
						},
					},
					Spec: v1beta1.IngressSpec{
						TLS: []v1beta1.IngressTLS{{
							Hosts:      []string{"www.example.com"},
							SecretName: "secret",
						}},
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
			},
			want: listenermap(&v2.Listener{
				Name:    ENVOY_HTTPS_LISTENER,
				Address: *envoy.SocketAddress("0.0.0.0", 8443),
				FilterChains: []listener.FilterChain{{
					FilterChainMatch: &listener.FilterChainMatch{
						ServerNames: []string{"www.example.com"},
					},
					TlsContext: tlscontext(auth.TlsParameters_TLSv1_1, "h2", "http/1.1"),
					Filters:    defaultfilters(),
				}},
				ListenerFilters: []listener.ListenerFilter{
					envoy.TLSInspector(),
				},
			}),
		},
		"http listener on non default port": { // issue 72
			ListenerVisitorConfig: ListenerVisitorConfig{
				HTTPAddress:  "127.0.0.100",
				HTTPPort:     9100,
				HTTPSAddress: "127.0.0.200",
				HTTPSPort:    9200,
			},
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						TLS: []v1beta1.IngressTLS{{
							Hosts:      []string{"whatever.example.com"},
							SecretName: "secret",
						}},
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
			},
			want: listenermap(&v2.Listener{
				Name:         ENVOY_HTTP_LISTENER,
				Address:      *envoy.SocketAddress("127.0.0.100", 9100),
				FilterChains: defaultfilterchain(),
			}, &v2.Listener{
				Name:    ENVOY_HTTPS_LISTENER,
				Address: *envoy.SocketAddress("127.0.0.200", 9200),
				ListenerFilters: []listener.ListenerFilter{
					envoy.TLSInspector(),
				},
				FilterChains: []listener.FilterChain{{
					FilterChainMatch: &listener.FilterChainMatch{
						ServerNames: []string{"whatever.example.com"},
					},
					TlsContext: tlscontext(auth.TlsParameters_TLSv1_1, "h2", "http/1.1"),
					Filters:    defaultfilters(),
				}},
			}),
		},
		"use proxy proto": {
			ListenerVisitorConfig: ListenerVisitorConfig{
				UseProxyProto: true,
			},
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						TLS: []v1beta1.IngressTLS{{
							Hosts:      []string{"whatever.example.com"},
							SecretName: "secret",
						}},
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
			},
			want: listenermap(&v2.Listener{
				Name:    ENVOY_HTTP_LISTENER,
				Address: *envoy.SocketAddress("0.0.0.0", 8080),
				ListenerFilters: []listener.ListenerFilter{
					envoy.ProxyProtocol(),
				},
				FilterChains: defaultfilterchain(),
			}, &v2.Listener{
				Name:    ENVOY_HTTPS_LISTENER,
				Address: *envoy.SocketAddress("0.0.0.0", 8443),
				ListenerFilters: []listener.ListenerFilter{
					envoy.ProxyProtocol(),
					envoy.TLSInspector(),
				},
				FilterChains: []listener.FilterChain{{
					FilterChainMatch: &listener.FilterChainMatch{
						ServerNames: []string{"whatever.example.com"},
					},
					TlsContext: tlscontext(auth.TlsParameters_TLSv1_1, "h2", "http/1.1"),
					Filters:    defaultfilters(),
				}},
			}),
		},
		"--envoy-http-access-log": {
			ListenerVisitorConfig: ListenerVisitorConfig{
				HTTPAccessLog:  "/tmp/http_access.log",
				HTTPSAccessLog: "/tmp/https_access.log",
			},
			objs: []interface{}{
				&v1beta1.Ingress{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "simple",
						Namespace: "default",
					},
					Spec: v1beta1.IngressSpec{
						TLS: []v1beta1.IngressTLS{{
							Hosts:      []string{"whatever.example.com"},
							SecretName: "secret",
						}},
						Backend: &v1beta1.IngressBackend{
							ServiceName: "kuard",
							ServicePort: intstr.FromInt(8080),
						},
					},
				},
				&v1.Secret{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "secret",
						Namespace: "default",
					},
					Type: "kubernetes.io/tls",
					Data: secretdata("certificate", "key"),
				},
			},
			want: listenermap(&v2.Listener{
				Name:    ENVOY_HTTP_LISTENER,
				Address: *envoy.SocketAddress(DEFAULT_HTTP_LISTENER_ADDRESS, DEFAULT_HTTP_LISTENER_PORT),
				FilterChains: filterchain(envoy.HTTPConnectionManager(ENVOY_HTTP_LISTENER, "/tmp/http_access.log",
					DEFAULT_RATE_LIMIT_SERVICE_DOMAIN, DEFAULT_RATE_LIMIT_SERVICE_STAGE, DEFAULT_RATE_LIMIT_SERVICE_FAILURE_MODE_DENY)),
			}, &v2.Listener{
				Name:    ENVOY_HTTPS_LISTENER,
				Address: *envoy.SocketAddress(DEFAULT_HTTPS_LISTENER_ADDRESS, DEFAULT_HTTPS_LISTENER_PORT),
				ListenerFilters: []listener.ListenerFilter{
					envoy.TLSInspector(),
				},
				FilterChains: []listener.FilterChain{{
					FilterChainMatch: &listener.FilterChainMatch{
						ServerNames: []string{"whatever.example.com"},
					},
					TlsContext: tlscontext(auth.TlsParameters_TLSv1_1, "h2", "http/1.1"),
					Filters: filters(envoy.HTTPConnectionManager(ENVOY_HTTPS_LISTENER, "/tmp/https_access.log",
						DEFAULT_RATE_LIMIT_SERVICE_DOMAIN, DEFAULT_RATE_LIMIT_SERVICE_STAGE, DEFAULT_RATE_LIMIT_SERVICE_FAILURE_MODE_DENY)),
				}},
			}),
		},
	}

	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			reh := ResourceEventHandler{
				FieldLogger: testLogger(t),
				Notifier:    new(nullNotifier),
				Metrics:     metrics.NewMetrics(prometheus.NewRegistry()),
			}
			for _, o := range tc.objs {
				reh.OnAdd(o)
			}
			root := dag.BuildDAG(&reh.KubernetesCache)
			got := visitListeners(root, &tc.ListenerVisitorConfig)
			if !cmp.Equal(tc.want, got) {
				t.Fatalf("expected:\n%+v\ngot:\n%+v", tc.want, got)
			}
		})
	}
}

func defaultfilters() []listener.Filter {
	return filters(envoy.HTTPConnectionManager(
		ENVOY_HTTPS_LISTENER,
		DEFAULT_HTTPS_ACCESS_LOG,
		DEFAULT_RATE_LIMIT_SERVICE_DOMAIN,
		DEFAULT_RATE_LIMIT_SERVICE_STAGE,
		DEFAULT_RATE_LIMIT_SERVICE_FAILURE_MODE_DENY))
}

func filters(first listener.Filter, rest ...listener.Filter) []listener.Filter {
	return append([]listener.Filter{first}, rest...)
}

func defaultfilterchain() []listener.FilterChain {
	return filterchain(envoy.HTTPConnectionManager(
		ENVOY_HTTP_LISTENER,
		DEFAULT_HTTP_ACCESS_LOG,
		DEFAULT_RATE_LIMIT_SERVICE_DOMAIN,
		DEFAULT_RATE_LIMIT_SERVICE_STAGE,
		DEFAULT_RATE_LIMIT_SERVICE_FAILURE_MODE_DENY))
}

func filterchain(filters ...listener.Filter) []listener.FilterChain {
	fc := listener.FilterChain{
		Filters: filters,
	}
	return []listener.FilterChain{fc}
}

func tlscontext(tlsMinProtoVersion auth.TlsParameters_TlsProtocol, alpnprotos ...string) *auth.DownstreamTlsContext {
	return envoy.DownstreamTLSContext("default/secret/735ad571c1", tlsMinProtoVersion, alpnprotos...)
}

func secretdata(cert, key string) map[string][]byte {
	return map[string][]byte{
		v1.TLSCertKey:       []byte(cert),
		v1.TLSPrivateKeyKey: []byte(key),
	}
}

func listenermap(listeners ...*v2.Listener) map[string]*v2.Listener {
	m := make(map[string]*v2.Listener)
	for _, l := range listeners {
		m[l.Name] = l
	}
	return m
}
