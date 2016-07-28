package main

import (
	"fmt"
	"io/ioutil"
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"github.com/go-kit/kit/log"
	"github.com/spf13/pflag"
	"golang.org/x/net/context"
	"k8s.io/kubernetes/pkg/client/restclient"

	"github.com/weaveworks/fluxy"
	"github.com/weaveworks/fluxy/automator"
	"github.com/weaveworks/fluxy/history"
	"github.com/weaveworks/fluxy/platform/kubernetes"
	"github.com/weaveworks/fluxy/registry"
)

func main() {
	// Flag domain.
	fs := pflag.NewFlagSet("default", pflag.ExitOnError)
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, "DESCRIPTION\n")
		fmt.Fprintf(os.Stderr, "  fluxd is a deployment daemon.\n")
		fmt.Fprintf(os.Stderr, "\n")
		fmt.Fprintf(os.Stderr, "FLAGS\n")
		fs.PrintDefaults()
	}
	// This mirrors how kubectl extracts information from the environment.
	var (
		listenAddr                = fs.StringP("listen", "l", ":3030", "Listen address for Flux API clients")
		registryCredentials       = fs.String("registry-credentials", "", "Path to image registry credentials file, in the format of ~/.docker/config.json")
		kubernetesKubectl         = fs.String("kubernetes-kubectl", "", "Optional, explicit path to kubectl tool")
		kubernetesHost            = fs.String("kubernetes-host", "", "Kubernetes host, e.g. http://10.11.12.13:8080")
		kubernetesUsername        = fs.String("kubernetes-username", "", "Kubernetes HTTP basic auth username")
		kubernetesPassword        = fs.String("kubernetes-password", "", "Kubernetes HTTP basic auth password")
		kubernetesClientCert      = fs.String("kubernetes-client-certificate", "", "Path to Kubernetes client certification file for TLS")
		kubernetesClientKey       = fs.String("kubernetes-client-key", "", "Path to Kubernetes client key file for TLS")
		kubernetesCertAuthority   = fs.String("kubernetes-certificate-authority", "", "Path to Kubernetes cert file for certificate authority")
		kubernetesBearerTokenFile = fs.String("kubernetes-bearer-token-file", "", "Path to file containing Kubernetes Bearer Token file")
		databaseDriver            = fs.String("database-driver", "", `Database driver name, e.g., "postgres"; if either this or --database-source are missing, an in-memory DB will be used`)
		databaseSource            = fs.String("database-source", "", `Database source name; specific to the database driver (--database-driver) used. If either of this or --database-driver are missing, an in-mem DB will be used`)
	)
	fs.Parse(os.Args)

	// Logger domain.
	var logger log.Logger
	{
		logger = log.NewLogfmtLogger(os.Stderr)
		logger = log.NewContext(logger).With("ts", log.DefaultTimestampUTC)
		logger = log.NewContext(logger).With("caller", log.DefaultCaller)
	}

	// Registry component.
	var reg *registry.Client
	{
		logger := log.NewContext(logger).With("component", "registry")
		creds := registry.NoCredentials()
		if *registryCredentials != "" {
			logger.Log("credentials", *registryCredentials)
			c, err := registry.CredentialsFromFile(*registryCredentials)
			if err != nil {
				logger.Log("err", err)
				os.Exit(1)
			}
			creds = c
		} else {
			logger.Log("credentials", "none")
		}
		reg = &registry.Client{
			Credentials: creds,
			Logger:      logger,
		}
	}

	// Platform component.
	var k8s *kubernetes.Cluster
	{
		// When adding a new platform, don't just bash it in. Create a Platform
		// or Cluster interface in package platform, and have kubernetes.Cluster
		// and your new platform implement that interface.
		logger := log.NewContext(logger).With("component", "platform")
		logger.Log("host", kubernetesHost)

		var bearerToken string
		if *kubernetesBearerTokenFile != "" {
			buf, err := ioutil.ReadFile(*kubernetesBearerTokenFile)
			if err != nil {
				logger.Log("err", err)
				os.Exit(1)
			}
			bearerToken = string(buf)
		}

		var err error
		k8s, err = kubernetes.NewCluster(&restclient.Config{
			Host:        *kubernetesHost,
			Username:    *kubernetesUsername,
			Password:    *kubernetesPassword,
			BearerToken: bearerToken,
			TLSClientConfig: restclient.TLSClientConfig{
				CertFile: *kubernetesClientCert,
				KeyFile:  *kubernetesClientKey,
				CAFile:   *kubernetesCertAuthority,
			},
		}, *kubernetesKubectl, logger)
		if err != nil {
			logger.Log("err", err)
			os.Exit(1)
		}

		if services, err := k8s.Services("default"); err != nil {
			logger.Log("services", err)
		} else {
			logger.Log("services", len(services))
		}
	}

	// History component.
	var his history.DB
	{
		if *databaseSource == "" || *databaseDriver == "" {
			his = history.NewInMemDB()
		} else {
			var err error
			his, err = history.NewSQL(*databaseDriver, *databaseSource,
				log.NewContext(logger).With("component", "history"))
			if err != nil {
				logger.Log("err", err)
				os.Exit(1)
			}
		}
	}

	// Automator component.
	var auto *automator.Automator
	{
		auto = automator.New(k8s, reg, his)
	}

	// Service (business logic) domain.
	var service flux.Service
	{
		service = flux.NewService(reg, k8s, auto, his, logger)
		service = flux.LoggingMiddleware(logger)(service)
	}

	// Endpoint domain.
	var endpoints flux.Endpoints
	{
		endpoints = flux.MakeServerEndpoints(service)
	}

	// Mechanical stuff.
	errc := make(chan error)
	go func() {
		c := make(chan os.Signal)
		signal.Notify(c, syscall.SIGINT, syscall.SIGTERM)
		errc <- fmt.Errorf("%s", <-c)
	}()

	// Transport domain.
	ctx := context.Background()
	go func() {
		logger := log.NewContext(logger).With("transport", "HTTP")
		logger.Log("addr", *listenAddr)
		h := flux.MakeHTTPHandler(ctx, endpoints, logger)
		errc <- http.ListenAndServe(*listenAddr, h)
	}()

	// Go!
	logger.Log("exit", <-errc)
}
