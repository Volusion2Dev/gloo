package e2e_test

import (
	"context"
	"fmt"
	"io/ioutil"
	"net"
	"net/http"
	"os"
	"strings"

	"github.com/solo-io/gloo/projects/gloo/pkg/api/v1/plugins/stats"

	. "github.com/onsi/ginkgo"
	. "github.com/onsi/gomega"
	"github.com/solo-io/gloo/test/services"
	"github.com/solo-io/go-utils/kubeutils"
	"github.com/solo-io/solo-kit/pkg/api/v1/clients"
	"github.com/solo-io/solo-kit/test/helpers"
	"github.com/solo-io/solo-kit/test/setup"

	gloov1 "github.com/solo-io/gloo/projects/gloo/pkg/api/v1"
	static_plugin_gloo "github.com/solo-io/gloo/projects/gloo/pkg/api/v1/plugins/static"
	"github.com/solo-io/gloo/projects/gloo/pkg/defaults"
	gloohelpers "github.com/solo-io/gloo/test/helpers"
	"github.com/solo-io/gloo/test/v1helpers"
	"github.com/solo-io/solo-kit/pkg/api/v1/resources/core"

	skkubeutils "github.com/solo-io/solo-kit/pkg/utils/kubeutils"
	kubev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	kubecore "k8s.io/kubernetes/pkg/apis/core"
	"k8s.io/kubernetes/pkg/apis/core/validation"
)

var _ = Describe("Happy path", func() {

	var (
		ctx           context.Context
		cancel        context.CancelFunc
		testClients   services.TestClients
		envoyInstance *services.EnvoyInstance
		tu            *v1helpers.TestUpstream
		envoyPort     uint32
	)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())
		defaults.HttpPort = services.NextBindPort()
		defaults.HttpsPort = services.NextBindPort()

		var err error
		envoyInstance, err = envoyFactory.NewEnvoyInstance()
		Expect(err).NotTo(HaveOccurred())

		tu = v1helpers.NewTestHttpUpstream(ctx, envoyInstance.LocalAddr())
		envoyPort = uint32(defaults.HttpPort)
	})

	AfterEach(func() {
		if envoyInstance != nil {
			_ = envoyInstance.Clean()
		}
		cancel()
	})

	TestUpstreamReachable := func() {
		v1helpers.TestUpstreamReachable(envoyPort, tu, nil)
	}

	Describe("in memory", func() {

		var up *gloov1.Upstream

		BeforeEach(func() {
			ns := defaults.GlooSystem
			ro := &services.RunOptions{
				NsToWrite: ns,
				NsToWatch: []string{"default", ns},
				WhatToRun: services.What{
					DisableGateway: true,
					DisableUds:     true,
					DisableFds:     true,
				},
			}
			testClients = services.RunGlooGatewayUdsFds(ctx, ro)
			err := envoyInstance.RunWithRole(ns+"~gateway-proxy", testClients.GlooPort)
			Expect(err).NotTo(HaveOccurred())

			up = tu.Upstream
			_, err = testClients.UpstreamClient.Write(up, clients.WriteOpts{})
			Expect(err).NotTo(HaveOccurred())
		})

		It("should not crash", func() {
			proxy := getTrivialProxyForUpstream(defaults.GlooSystem, envoyPort, up.Metadata.Ref())
			_, err := testClients.ProxyClient.Write(proxy, clients.WriteOpts{})
			Expect(err).NotTo(HaveOccurred())

			TestUpstreamReachable()
		})

		It("correctly configures envoy to emit virtual cluster statistics", func() {
			proxy := getTrivialProxyForUpstream(defaults.GlooSystem, envoyPort, up.Metadata.Ref())

			// Set a virtual cluster matching everything
			proxy.Listeners[0].GetHttpListener().VirtualHosts[0].VirtualHostPlugins = &gloov1.VirtualHostPlugins{
				Stats: &stats.Stats{
					VirtualClusters: []*stats.VirtualCluster{{
						Name:    "test-vc",
						Pattern: ".*",
					}},
				},
			}

			_, err := testClients.ProxyClient.Write(proxy, clients.WriteOpts{})
			Expect(err).NotTo(HaveOccurred())

			// This will hit the virtual host with the above virtual cluster config
			TestUpstreamReachable()

			response, err := http.Get(fmt.Sprintf("http://localhost:%d/stats", envoyInstance.AdminPort))
			Expect(err).NotTo(HaveOccurred())
			Expect(response).NotTo(BeNil())
			//noinspection GoUnhandledErrorResult
			defer response.Body.Close()

			body, err := ioutil.ReadAll(response.Body)
			Expect(err).NotTo(HaveOccurred())
			statsString := string(body)

			// Verify that stats for the above virtual cluster are present
			Expect(statsString).To(ContainSubstring("vhost.virt1.vcluster.test-vc."))
		})

		Context("ssl", func() {
			var upSsl *gloov1.Upstream

			BeforeEach(func() {

				sslSecret := &gloov1.Secret{
					Metadata: core.Metadata{
						Name:      "secret",
						Namespace: "default",
					},
					Kind: &gloov1.Secret_Tls{
						Tls: &gloov1.TlsSecret{
							RootCa: gloohelpers.Certificate(),
						},
					},
				}
				_, err := testClients.SecretClient.Write(sslSecret, clients.WriteOpts{})
				Expect(err).NotTo(HaveOccurred())
				// create ssl proxy
				copyUp := *tu.Upstream
				copyUp.Metadata.Name = copyUp.Metadata.Name + "-ssl"
				port := tu.Upstream.UpstreamSpec.UpstreamType.(*gloov1.UpstreamSpec_Static).Static.Hosts[0].Port
				addr := tu.Upstream.UpstreamSpec.UpstreamType.(*gloov1.UpstreamSpec_Static).Static.Hosts[0].Addr
				sslport := v1helpers.StartSslProxy(ctx, port)
				ref := sslSecret.Metadata.Ref()

				copyUp.UpstreamSpec = &gloov1.UpstreamSpec{
					UpstreamType: &gloov1.UpstreamSpec_Static{
						Static: &static_plugin_gloo.UpstreamSpec{
							Hosts: []*static_plugin_gloo.Host{{
								Addr: addr,
								Port: sslport,
							}},
						},
					},
				}
				copyUp.UpstreamSpec.SslConfig = &gloov1.UpstreamSslConfig{
					SslSecrets: &gloov1.UpstreamSslConfig_SecretRef{
						SecretRef: &ref,
					},
				}
				upSsl = &copyUp
				_, err = testClients.UpstreamClient.Write(upSsl, clients.WriteOpts{})
				Expect(err).NotTo(HaveOccurred())
			})

			It("should work with ssl", func() {
				proxycli := testClients.ProxyClient
				proxy := getTrivialProxyForUpstream(defaults.GlooSystem, envoyPort, upSsl.Metadata.Ref())
				_, err := proxycli.Write(proxy, clients.WriteOpts{})
				Expect(err).NotTo(HaveOccurred())

				TestUpstreamReachable()
			})
		})

		Context("sad path", func() {
			It("should error the proxy with two listeners with the same bind address", func() {

				proxycli := testClients.ProxyClient
				proxy := getTrivialProxyForUpstream(defaults.GlooSystem, envoyPort, up.Metadata.Ref())
				// add two identical listeners two see errors come up
				proxy.Listeners = append(proxy.Listeners, proxy.Listeners[0])
				_, err := proxycli.Write(proxy, clients.WriteOpts{})
				Expect(err).NotTo(HaveOccurred())

				getStatus := func() (core.Status_State, error) {
					updatedProxy, err := proxycli.Read(proxy.Metadata.Namespace, proxy.Metadata.Name, clients.ReadOpts{})
					if err != nil {
						return 0, err
					}
					return updatedProxy.Status.State, nil
				}

				Eventually(getStatus, "10s").ShouldNot(Equal(core.Status_Pending))
				st, err := getStatus()
				Expect(err).NotTo(HaveOccurred())
				Expect(st).To(Equal(core.Status_Rejected))

			})
		})
	})

	Describe("kubernetes happy path", func() {
		BeforeEach(func() {
			if os.Getenv("RUN_KUBE_TESTS") != "1" {
				Skip("This test creates kubernetes resources and is disabled by default. To enable, set RUN_KUBE_TESTS=1 in your env.")
			}
		})

		var (
			namespace      string
			writeNamespace string
			cfg            *rest.Config
			kubeClient     kubernetes.Interface
			svc            *kubev1.Service
		)

		BeforeEach(func() {
			namespace = ""
			writeNamespace = ""
			var err error
			svc = nil
			cfg, err = kubeutils.GetConfig("", "")
			Expect(err).NotTo(HaveOccurred())
			kubeClient, err = kubernetes.NewForConfig(cfg)
			Expect(err).NotTo(HaveOccurred())
		})

		AfterEach(func() {
			if namespace != "" {
				_ = setup.TeardownKube(namespace)
			}
		})

		prepNamespace := func() {
			if namespace == "" {
				namespace = "gloo-e2e-" + helpers.RandString(8)
			}

			err := setup.SetupKubeForTest(namespace)
			Expect(err).NotTo(HaveOccurred())

			svc, err = kubeClient.CoreV1().Services(namespace).Create(&kubev1.Service{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      "headlessservice",
				},
				Spec: kubev1.ServiceSpec{
					Ports: []kubev1.ServicePort{
						{
							Name: "foo",
							Port: int32(tu.Port),
						},
					},
				},
			})
			Expect(err).NotTo(HaveOccurred())

			_, err = kubeClient.CoreV1().Endpoints(namespace).Create(&kubev1.Endpoints{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: namespace,
					Name:      svc.Name,
				},
				Subsets: []kubev1.EndpointSubset{{
					Addresses: []kubev1.EndpointAddress{{
						IP:       getIpThatsNotLocalhost(),
						Hostname: "localhost",
					}},
					Ports: []kubev1.EndpointPort{{
						Port: int32(tu.Port),
					}},
				}},
			})
			Expect(err).NotTo(HaveOccurred())
		}

		getUpstream := func() (*gloov1.Upstream, error) {
			l, err := testClients.UpstreamClient.List(writeNamespace, clients.ListOpts{})
			if err != nil {
				return nil, err
			}
			for _, u := range l {
				if strings.Contains(u.Metadata.Name, svc.Name) && strings.Contains(u.Metadata.Name, svc.Namespace) {
					return u, nil
				}
			}
			return nil, fmt.Errorf("not found")
		}

		getStatus := func() (core.Status_State, error) {
			u, err := getUpstream()
			if err != nil {
				return core.Status_Pending, err
			}
			return u.Status.State, nil
		}

		Context("specific namespace", func() {

			BeforeEach(func() {
				prepNamespace()
				writeNamespace = namespace
				ro := &services.RunOptions{
					NsToWrite: writeNamespace,
					NsToWatch: []string{"default", namespace},
					WhatToRun: services.What{
						DisableGateway: true,
					},
					KubeClient: kubeClient,
				}

				testClients = services.RunGlooGatewayUdsFds(ctx, ro)
				role := namespace + "~gateway-proxy"
				err := envoyInstance.RunWithRole(role, testClients.GlooPort)
				Expect(err).NotTo(HaveOccurred())

				Eventually(getStatus, "20s", "0.5s").Should(Equal(core.Status_Accepted))
			})

			It("should discover service", func() {
				up, err := getUpstream()
				Expect(err).NotTo(HaveOccurred())

				proxycli := testClients.ProxyClient
				proxy := getTrivialProxyForUpstream(namespace, envoyPort, up.Metadata.Ref())
				var opts clients.WriteOpts
				_, err = proxycli.Write(proxy, opts)
				Expect(err).NotTo(HaveOccurred())

				TestUpstreamReachable()
			})

			It("correctly routes requests to a service destination", func() {
				svcRef := skkubeutils.FromKubeMeta(svc.ObjectMeta).Ref()
				svcPort := svc.Spec.Ports[0].Port
				proxy := getTrivialProxyForService(namespace, envoyPort, svcRef, uint32(svcPort))

				_, err := testClients.ProxyClient.Write(proxy, clients.WriteOpts{})
				Expect(err).NotTo(HaveOccurred())

				TestUpstreamReachable()
			})
		})

		Context("all namespaces", func() {
			BeforeEach(func() {
				namespace = "gloo-e2e-" + helpers.RandString(8)

				writeNamespace = defaults.GlooSystem
				ro := &services.RunOptions{
					NsToWrite: writeNamespace,
					NsToWatch: []string{},
					WhatToRun: services.What{
						DisableGateway: true,
					},
					KubeClient: kubeClient,
				}

				testClients = services.RunGlooGatewayUdsFds(ctx, ro)
				role := namespace + "~gateway-proxy"
				err := envoyInstance.RunWithRole(role, testClients.GlooPort)
				Expect(err).NotTo(HaveOccurred())

				prepNamespace()
			})

			It("watch all namespaces", func() {
				Eventually(getStatus, "20s", "0.5s").Should(Equal(core.Status_Accepted))

				up, err := getUpstream()
				Expect(err).NotTo(HaveOccurred())

				proxycli := testClients.ProxyClient
				proxy := getTrivialProxyForUpstream(namespace, envoyPort, up.Metadata.Ref())
				var opts clients.WriteOpts
				_, err = proxycli.Write(proxy, opts)
				Expect(err).NotTo(HaveOccurred())

				TestUpstreamReachable()
			})
		})
	})
})

func getTrivialProxyForUpstream(ns string, bindPort uint32, upstream core.ResourceRef) *gloov1.Proxy {
	proxy := getTrivialProxy(ns, bindPort)
	proxy.Listeners[0].ListenerType.(*gloov1.Listener_HttpListener).HttpListener.
		VirtualHosts[0].Routes[0].Action.(*gloov1.Route_RouteAction).RouteAction.
		Destination.(*gloov1.RouteAction_Single).Single.DestinationType =
		&gloov1.Destination_Upstream{Upstream: &upstream}
	return proxy
}

func getTrivialProxyForService(ns string, bindPort uint32, service core.ResourceRef, svcPort uint32) *gloov1.Proxy {
	proxy := getTrivialProxy(ns, bindPort)
	proxy.Listeners[0].ListenerType.(*gloov1.Listener_HttpListener).HttpListener.
		VirtualHosts[0].Routes[0].Action.(*gloov1.Route_RouteAction).RouteAction.
		Destination.(*gloov1.RouteAction_Single).Single.DestinationType =
		&gloov1.Destination_Service{
			Service: &gloov1.ServiceDestination{
				Ref:  service,
				Port: svcPort,
			},
		}
	return proxy
}

func getTrivialProxy(ns string, bindPort uint32) *gloov1.Proxy {
	return &gloov1.Proxy{
		Metadata: core.Metadata{
			Name:      "gateway-proxy",
			Namespace: ns,
		},
		Listeners: []*gloov1.Listener{{
			Name:        "listener",
			BindAddress: "::",
			BindPort:    bindPort,
			ListenerType: &gloov1.Listener_HttpListener{
				HttpListener: &gloov1.HttpListener{
					VirtualHosts: []*gloov1.VirtualHost{{
						Name:    "virt1",
						Domains: []string{"*"},
						Routes: []*gloov1.Route{{
							Matcher: &gloov1.Matcher{
								PathSpecifier: &gloov1.Matcher_Prefix{
									Prefix: "/",
								},
							},
							Action: &gloov1.Route_RouteAction{
								RouteAction: &gloov1.RouteAction{
									Destination: &gloov1.RouteAction_Single{
										Single: &gloov1.Destination{},
									},
								},
							},
						}},
					}},
				},
			},
		}},
	}
}

func getIpThatsNotLocalhost() string {
	// kubernetes endpoints doesn't like localhost, so we just give it some other local address
	// from: k8s.io/kubernetes/pkg/apis/core/validation/validation.go
	/*
		func validateNonSpecialIP(ipAddress string, fldPath *field.Path) field.ErrorList {
		        // We disallow some IPs as endpoints or external-ips.  Specifically,
		        // unspecified and loopback addresses are nonsensical and link-local
		        // addresses tend to be used for node-centric purposes (e.g. metadata
		        // service).
	*/
	ifaces, err := net.Interfaces()
	ExpectWithOffset(1, err).NotTo(HaveOccurred())

	for _, iface := range ifaces {
		if iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			default:
				continue
			}

			// make sure that kubernetes like this endpoint:
			endpoints := &kubecore.Endpoints{
				ObjectMeta: metav1.ObjectMeta{
					Namespace: "validate",
					Name:      "validate",
				},
				Subsets: []kubecore.EndpointSubset{{
					Addresses: []kubecore.EndpointAddress{{
						IP:       ip.String(),
						Hostname: "localhost",
					}},
					Ports: []kubecore.EndpointPort{{
						Port:     int32(5555),
						Protocol: kubecore.ProtocolTCP,
					}},
				}},
			}

			errs := validation.ValidateEndpoints(endpoints)
			if len(errs) != 0 {
				continue
			}

			return ip.String()
		}
	}
	Fail("no ip address available", 1)
	return ""
}
