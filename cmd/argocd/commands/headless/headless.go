package headless

import (
	"context"
	"fmt"
	"net"
	"os"
	"sync"
	"time"

	"github.com/alicebob/miniredis/v2"
	"github.com/golang/protobuf/ptypes/empty"
	"github.com/redis/go-redis/v9"
	log "github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	runtimeUtil "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	cache2 "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/argoproj/argo-cd/v3/cmd/argocd/commands/initialize"
	"github.com/argoproj/argo-cd/v3/common"
	"github.com/argoproj/argo-cd/v3/pkg/apiclient"
	"github.com/argoproj/argo-cd/v3/pkg/apis/application/v1alpha1"
	appclientset "github.com/argoproj/argo-cd/v3/pkg/client/clientset/versioned"
	repoapiclient "github.com/argoproj/argo-cd/v3/reposerver/apiclient"
	"github.com/argoproj/argo-cd/v3/server"
	servercache "github.com/argoproj/argo-cd/v3/server/cache"
	"github.com/argoproj/argo-cd/v3/util/cache"
	appstatecache "github.com/argoproj/argo-cd/v3/util/cache/appstate"
	"github.com/argoproj/argo-cd/v3/util/cli"
	utilio "github.com/argoproj/argo-cd/v3/util/io"
	kubeutil "github.com/argoproj/argo-cd/v3/util/kube"
	"github.com/argoproj/argo-cd/v3/util/localconfig"
)

type forwardCacheClient struct {
	namespace        string
	context          string
	init             sync.Once
	client           cache.CacheClient
	compression      cache.RedisCompressionType
	err              error
	redisHaProxyName string
	redisName        string
	redisPassword    string
}

func (c *forwardCacheClient) doLazy(action func(client cache.CacheClient) error) error {
	c.init.Do(func() {
		overrides := clientcmd.ConfigOverrides{
			CurrentContext: c.context,
		}
		redisHaProxyPodLabelSelector := common.LabelKeyAppName + "=" + c.redisHaProxyName
		redisPodLabelSelector := common.LabelKeyAppName + "=" + c.redisName
		redisPort, err := kubeutil.PortForward(6379, c.namespace, &overrides,
			redisHaProxyPodLabelSelector, redisPodLabelSelector)
		if err != nil {
			c.err = err
			return
		}

		redisClient := redis.NewClient(&redis.Options{Addr: fmt.Sprintf("localhost:%d", redisPort), Password: c.redisPassword})
		c.client = cache.NewRedisCache(redisClient, time.Hour, c.compression)
	})
	if c.err != nil {
		return c.err
	}
	return action(c.client)
}

func (c *forwardCacheClient) Set(item *cache.Item) error {
	return c.doLazy(func(client cache.CacheClient) error {
		return client.Set(item)
	})
}

func (c *forwardCacheClient) Rename(oldKey string, newKey string, expiration time.Duration) error {
	return c.doLazy(func(client cache.CacheClient) error {
		return client.Rename(oldKey, newKey, expiration)
	})
}

func (c *forwardCacheClient) Get(key string, obj any) error {
	return c.doLazy(func(client cache.CacheClient) error {
		return client.Get(key, obj)
	})
}

func (c *forwardCacheClient) Delete(key string) error {
	return c.doLazy(func(client cache.CacheClient) error {
		return client.Delete(key)
	})
}

func (c *forwardCacheClient) OnUpdated(ctx context.Context, key string, callback func() error) error {
	return c.doLazy(func(client cache.CacheClient) error {
		return client.OnUpdated(ctx, key, callback)
	})
}

func (c *forwardCacheClient) NotifyUpdated(key string) error {
	return c.doLazy(func(client cache.CacheClient) error {
		return client.NotifyUpdated(key)
	})
}

type forwardRepoClientset struct {
	namespace      string
	context        string
	init           sync.Once
	repoClientset  repoapiclient.Clientset
	err            error
	repoServerName string
	kubeClientset  kubernetes.Interface
}

func (c *forwardRepoClientset) NewRepoServerClient() (utilio.Closer, repoapiclient.RepoServerServiceClient, error) {
	c.init.Do(func() {
		overrides := clientcmd.ConfigOverrides{
			CurrentContext: c.context,
		}
		repoServerName := c.repoServerName
		repoServererviceLabelSelector := common.LabelKeyComponentRepoServer + "=" + common.LabelValueComponentRepoServer
		repoServerServices, err := c.kubeClientset.CoreV1().Services(c.namespace).List(context.Background(), metav1.ListOptions{LabelSelector: repoServererviceLabelSelector})
		if err != nil {
			c.err = err
			return
		}
		if len(repoServerServices.Items) > 0 {
			if repoServerServicelabel, ok := repoServerServices.Items[0].Labels[common.LabelKeyAppName]; ok && repoServerServicelabel != "" {
				repoServerName = repoServerServicelabel
			}
		}
		repoServerPodLabelSelector := common.LabelKeyAppName + "=" + repoServerName
		repoServerPort, err := kubeutil.PortForward(8081, c.namespace, &overrides, repoServerPodLabelSelector)
		if err != nil {
			c.err = err
			return
		}
		c.repoClientset = repoapiclient.NewRepoServerClientset(fmt.Sprintf("localhost:%d", repoServerPort), 60, repoapiclient.TLSConfiguration{
			DisableTLS: false, StrictValidation: false,
		})
	})
	if c.err != nil {
		return nil, nil, c.err
	}
	return c.repoClientset.NewRepoServerClient()
}

func testAPI(ctx context.Context, clientOpts *apiclient.ClientOptions) error {
	apiClient, err := apiclient.NewClient(clientOpts)
	if err != nil {
		return fmt.Errorf("failed to create API client: %w", err)
	}
	closer, versionClient, err := apiClient.NewVersionClient()
	if err != nil {
		return fmt.Errorf("failed to create version client: %w", err)
	}
	defer utilio.Close(closer)
	_, err = versionClient.Version(ctx, &empty.Empty{})
	if err != nil {
		return fmt.Errorf("failed to get version: %w", err)
	}
	return nil
}

// MaybeStartLocalServer allows executing command in a headless mode. If we're in core mode, starts the Argo CD API
// server on the fly and changes provided client options to use started API server port.
//
// If the clientOpts enables core mode, but the local config does not have core mode enabled, this function will
// not start the local server.
func MaybeStartLocalServer(ctx context.Context, clientOpts *apiclient.ClientOptions, ctxStr string, port *int, address *string, clientConfig clientcmd.ClientConfig) (func(), error) {
	if clientConfig == nil {
		flags := pflag.NewFlagSet("tmp", pflag.ContinueOnError)
		clientConfig = cli.AddKubectlFlagsToSet(flags)
	}
	startInProcessAPI := clientOpts.Core
	if !startInProcessAPI {
		// Core mode is enabled on client options. Check the local config to see if we should start the API server.
		localCfg, err := localconfig.ReadLocalConfig(clientOpts.ConfigPath)
		if err != nil {
			return nil, fmt.Errorf("error reading local config: %w", err)
		}
		if localCfg != nil {
			configCtx, err := localCfg.ResolveContext(clientOpts.Context)
			if err != nil {
				return nil, fmt.Errorf("error resolving context: %w", err)
			}
			// There was a local config file, so determine whether core mode is enabled per the config file.
			startInProcessAPI = configCtx.Server.Core
		}
	}
	// If we're in core mode, start the API server on the fly.
	if !startInProcessAPI {
		return nil, nil
	}

	// get rid of logging error handler
	runtimeUtil.ErrorHandlers = runtimeUtil.ErrorHandlers[1:]
	cli.SetLogLevel(log.ErrorLevel.String())
	log.SetLevel(log.ErrorLevel)
	os.Setenv(v1alpha1.EnvVarFakeInClusterConfig, "true")
	if address == nil {
		address = ptr.To("localhost")
	}
	if port == nil || *port == 0 {
		addr := *address + ":0"
		ln, err := net.Listen("tcp", addr)
		if err != nil {
			return nil, fmt.Errorf("failed to listen on %q: %w", addr, err)
		}
		port = &ln.Addr().(*net.TCPAddr).Port
		utilio.Close(ln)
	}

	restConfig, err := clientConfig.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("error creating client config: %w", err)
	}
	appClientset, err := appclientset.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating app clientset: %w", err)
	}
	kubeClientset, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes clientset: %w", err)
	}

	dynamicClientset, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes dynamic clientset: %w", err)
	}

	scheme := runtime.NewScheme()
	err = v1alpha1.AddToScheme(scheme)
	if err != nil {
		return nil, fmt.Errorf("error adding argo resources to scheme: %w", err)
	}
	err = corev1.AddToScheme(scheme)
	if err != nil {
		return nil, fmt.Errorf("error adding corev1 resources to scheme: %w", err)
	}
	controllerClientset, err := client.New(restConfig, client.Options{
		Scheme: scheme,
	})
	if err != nil {
		return nil, fmt.Errorf("error creating kubernetes controller clientset: %w", err)
	}
	controllerClientset = client.NewDryRunClient(controllerClientset)

	namespace, _, err := clientConfig.Namespace()
	if err != nil {
		return nil, fmt.Errorf("error getting namespace: %w", err)
	}

	mr, err := miniredis.Run()
	if err != nil {
		return nil, fmt.Errorf("error running miniredis: %w", err)
	}
	redisOptions := &redis.Options{Addr: mr.Addr()}
	if err = common.SetOptionalRedisPasswordFromKubeConfig(ctx, kubeClientset, namespace, redisOptions); err != nil {
		log.Warnf("Failed to fetch & set redis password for namespace %s: %v", namespace, err)
	}

	appstateCache := appstatecache.NewCache(cache.NewCache(&forwardCacheClient{namespace: namespace, context: ctxStr, compression: cache.RedisCompressionType(clientOpts.RedisCompression), redisHaProxyName: clientOpts.RedisHaProxyName, redisName: clientOpts.RedisName, redisPassword: redisOptions.Password}), time.Hour)
	srv := server.NewServer(ctx, server.ArgoCDServerOpts{
		EnableGZip:              false,
		Namespace:               namespace,
		ListenPort:              *port,
		AppClientset:            appClientset,
		DisableAuth:             true,
		RedisClient:             redis.NewClient(redisOptions),
		Cache:                   servercache.NewCache(appstateCache, 0, 0),
		KubeClientset:           kubeClientset,
		DynamicClientset:        dynamicClientset,
		KubeControllerClientset: controllerClientset,
		Insecure:                true,
		ListenHost:              *address,
		RepoClientset:           &forwardRepoClientset{namespace: namespace, context: ctxStr, repoServerName: clientOpts.RepoServerName, kubeClientset: kubeClientset},
		EnableProxyExtension:    false,
	}, server.ApplicationSetOpts{})
	srv.Init(ctx)

	lns, err := srv.Listen()
	if err != nil {
		return nil, fmt.Errorf("failed to listen: %w", err)
	}
	go srv.Run(ctx, lns)
	clientOpts.ServerAddr = fmt.Sprintf("%s:%d", *address, *port)
	clientOpts.PlainText = true
	if !cache2.WaitForCacheSync(ctx.Done(), srv.Initialized) {
		log.Fatal("Timed out waiting for project cache to sync")
	}

	tries := 5
	for i := 0; i < tries; i++ {
		err = testAPI(ctx, clientOpts)
		if err == nil {
			break
		}
		time.Sleep(time.Second)
	}
	if err != nil {
		return nil, fmt.Errorf("all retries failed: %w", err)
	}
	return srv.Shutdown, nil
}

// NewClientOrDie creates a new API client from a set of config options, or fails fatally if the new client creation fails.
func NewClientOrDie(opts *apiclient.ClientOptions, c *cobra.Command) apiclient.Client {
	ctx := c.Context()

	ctxStr := initialize.RetrieveContextIfChanged(c.Flag("context"))
	// If we're in core mode, start the API server on the fly and configure the client `opts` to use it.
	// If we're not in core mode, this function call will do nothing.
	_, err := MaybeStartLocalServer(ctx, opts, ctxStr, nil, nil, nil)
	if err != nil {
		log.Fatal(err)
	}
	client, err := apiclient.NewClient(opts)
	if err != nil {
		log.Fatal(err)
	}
	return client
}
