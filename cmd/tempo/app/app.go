package app

import (
	"context"
	"flag"
	"fmt"

	"github.com/cortexproject/cortex/pkg/ring"
	"github.com/cortexproject/cortex/pkg/ring/kv/memberlist"
	"github.com/cortexproject/cortex/pkg/util"
	"github.com/cortexproject/cortex/pkg/util/modules"
	"github.com/cortexproject/cortex/pkg/util/services"
	"github.com/go-kit/kit/log/level"

	"github.com/weaveworks/common/middleware"
	"github.com/weaveworks/common/server"
	"github.com/weaveworks/common/signals"
	"google.golang.org/grpc"

	"github.com/grafana/tempo/pkg/compactor"
	"github.com/grafana/tempo/pkg/distributor"
	"github.com/grafana/tempo/pkg/ingester"
	ingester_client "github.com/grafana/tempo/pkg/ingester/client"
	"github.com/grafana/tempo/pkg/querier"
	"github.com/grafana/tempo/pkg/storage"
	"github.com/grafana/tempo/pkg/util/validation"
)

// Config is the root config for App.
type Config struct {
	Target      string `yaml:"target,omitempty"`
	AuthEnabled bool   `yaml:"auth_enabled,omitempty"`
	HTTPPrefix  string `yaml:"http_prefix"`

	Server         server.Config          `yaml:"server,omitempty"`
	Distributor    distributor.Config     `yaml:"distributor,omitempty"`
	IngesterClient ingester_client.Config `yaml:"ingester_client,omitempty"`
	Querier        querier.Config         `yaml:"querier,omitempty"`
	Compactor      compactor.Config       `yaml:"compactor,omitempty"`
	Ingester       ingester.Config        `yaml:"ingester,omitempty"`
	StorageConfig  storage.Config         `yaml:"storage_config,omitempty"`
	LimitsConfig   validation.Limits      `yaml:"limits_config,omitempty"`
	MemberlistKV   memberlist.KVConfig    `yaml:"memberlist,omitempty"`
}

// RegisterFlags registers flag.
func (c *Config) RegisterFlags(f *flag.FlagSet) {
	const metricsNamespace = "tempo"

	c.Server.MetricsNamespace = metricsNamespace       // jpe can use metircs namespace? move all this config to inits?
	c.MemberlistKV.MetricsNamespace = metricsNamespace // jpe use .MetricsRegisterer
	c.Target = All
	c.Server.ExcludeRequestInLog = true
	f.StringVar(&c.Target, "target", "target module (default All)")
	f.BoolVar(&c.AuthEnabled, "auth.enabled", true, "Set to false to disable auth.")

	c.Server.RegisterFlags(f)
	c.Distributor.RegisterFlags(f)
	c.IngesterClient.RegisterFlags(f)
	c.Querier.RegisterFlags(f)
	c.Compactor.RegisterFlags(f)
	c.Ingester.RegisterFlags(f)
	c.StorageConfig.RegisterFlags(f)
	c.LimitsConfig.RegisterFlags(f)
}

// App is the root datastructure.
type App struct {
	cfg Config

	server       *server.Server
	ring         *ring.Ring
	overrides    *validation.Overrides
	distributor  *distributor.Distributor
	querier      *querier.Querier
	compactor    *compactor.Compactor
	ingester     *ingester.Ingester
	store        storage.Store
	memberlistKV *memberlist.KVInitService

	httpAuthMiddleware middleware.Interface
	moduleManager      *modules.Manager
	serviceMap         map[string]services.Service
}

// New makes a new app.
func New(cfg Config) (*App, error) {
	app := &App{
		cfg: cfg,
	}

	app.setupAuthMiddleware()

	if err := app.setupModuleManager(); err != nil {
		return nil, fmt.Errorf("failed to setup module manager %w", err)
	}

	return app, nil
}

func (t *App) setupAuthMiddleware() {
	if t.cfg.AuthEnabled {
		t.cfg.Server.GRPCMiddleware = []grpc.UnaryServerInterceptor{
			middleware.ServerUserHeaderInterceptor,
		}
		t.cfg.Server.GRPCStreamMiddleware = []grpc.StreamServerInterceptor{
			func(srv interface{}, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
				return middleware.StreamServerUserHeaderInterceptor(srv, ss, info, handler)
			},
		}
		t.httpAuthMiddleware = middleware.AuthenticateUser
	} else {
		t.cfg.Server.GRPCMiddleware = []grpc.UnaryServerInterceptor{
			fakeGRPCAuthUniaryMiddleware,
		}
		t.cfg.Server.GRPCStreamMiddleware = []grpc.StreamServerInterceptor{
			fakeGRPCAuthStreamMiddleware,
		}
		t.httpAuthMiddleware = fakeHTTPAuthMiddleware
	}
}

// Run starts, and blocks until a signal is received.
func (t *App) Run() error {
	if !t.moduleManager.IsUserVisibleModule(t.cfg.Target) {
		level.Warn(util.Logger).Log("msg", "selected target is an internal module, is this intended?", "target", t.cfg.Target)
	}

	serviceMap, err := t.moduleManager.InitModuleServices(t.cfg.Target)
	if err != nil {
		return fmt.Errorf("failed to init module services %w", err)
	}
	t.serviceMap = serviceMap

	servs := []services.Service(nil)
	for _, s := range serviceMap {
		servs = append(servs, s)
	}

	sm, err := services.NewManager(servs...)
	if err != nil {
		return fmt.Errorf("failed to start service manager %w", err)
	}

	// Let's listen for events from this manager, and log them.
	healthy := func() { level.Info(util.Logger).Log("msg", "Cortex started") }
	stopped := func() { level.Info(util.Logger).Log("msg", "Cortex stopped") }
	serviceFailed := func(service services.Service) {
		// if any service fails, stop entire Cortex
		sm.StopAsync()

		// let's find out which module failed
		for m, s := range serviceMap {
			if s == service {
				if service.FailureCase() == util.ErrStopProcess {
					level.Info(util.Logger).Log("msg", "received stop signal via return error", "module", m, "err", service.FailureCase())
				} else {
					level.Error(util.Logger).Log("msg", "module failed", "module", m, "err", service.FailureCase())
				}
				return
			}
		}

		level.Error(util.Logger).Log("msg", "module failed", "module", "unknown", "err", service.FailureCase())
	}
	sm.AddListener(services.NewManagerListener(healthy, stopped, serviceFailed))

	// Setup signal handler. If signal arrives, we stop the manager, which stops all the services.
	handler := signals.NewHandler(t.server.Log)
	go func() {
		handler.Loop()
		sm.StopAsync()
	}()

	// Start all services. This can really only fail if some service is already
	// in other state than New, which should not be the case.
	err = sm.StartAsync(context.Background())
	if err != nil {
		return fmt.Errorf("failed to start service manager %w", err)
	}

	return sm.AwaitStopped(context.Background())
}