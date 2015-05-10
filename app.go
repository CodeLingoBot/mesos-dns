package main

import (
	"time"

	"github.com/emicklei/go-restful"
	"github.com/mesosphere/mesos-dns/logging"
	"github.com/mesosphere/mesos-dns/plugins"
	"github.com/mesosphere/mesos-dns/records"
	"github.com/mesosphere/mesos-dns/resolver"
	"github.com/mesosphere/mesos-dns/util"
)

const (
	zkInitialDetectionTimeout = 30 * time.Second
)

type errorHandlerFunc func(string, error)

type app struct {
	config     records.Config
	resolver   *resolver.Resolver
	filters    plugins.FilterSet
	ready      chan struct{} // when closed, indicates that initialization has completed
	done       chan struct{} // when closed, indicates that run has completed
	errHandler errorHandlerFunc
}

type pluginContext struct {
	*app
	pluginName string
}

func newApp(eh errorHandlerFunc) *app {
	c := &app{
		errHandler: eh,
		ready:      make(chan struct{}),
		done:       make(chan struct{}),
	}
	c.initialize()
	return c
}

func (c *app) Resolver() plugins.Resolver {
	return c
}

func (c *app) Done() <-chan struct{} {
	// clients that use this chan will block until run completes
	return c.done
}

// implements plugin.Resolver interface, panics if invoked outside of initialization process
func (c *app) OnPreload(r plugins.Reloader) {
	select {
	case <-c.ready:
		panic("cannot OnPreload after initialization has completed")
	default:
		c.resolver.OnPreload(resolver.RecordLoader(r))
	}
}

func (c *app) OnPostload(r plugins.Reloader) {
	select {
	case <-c.ready:
		panic("cannot OnPostload after initialization has completed")
	default:
		c.resolver.OnPostload(resolver.RecordLoader(r))
	}
}

func (c *app) AddFilter(f plugins.Filter) {
	select {
	case <-c.ready:
		panic("cannot AddFilter after initialization has completed")
	default:
	}
	if f != nil {
		c.filters = append(c.filters, f)
	}
}

func (c *app) RegisterWS(ws *restful.WebService) {
	select {
	case <-c.ready:
		panic("cannot RegisterWS after initialization has completed")
	default:
		restful.Add(ws)
	}
}

// return a clone of the global configuration, minus any plugin-specific JSON
func (c *app) Config() *records.Config {
	cfg := c.config
	cfg.Plugins = nil
	cfg.Masters = make([]string, len(c.config.Masters))
	copy(cfg.Masters, c.config.Masters)
	cfg.Resolvers = make([]string, len(c.config.Resolvers))
	copy(cfg.Resolvers, c.config.Resolvers)
	return &cfg
}

func (c *app) initialize() {
	select {
	case <-c.ready:
		panic("app already initialized")
	default:
		defer close(c.ready)
	}

	c.config = records.SetConfig(*cjson)
	c.resolver = resolver.New(version, c.config)

	// launch built-in plugins
	if c.config.HttpOn {
		c.launchPlugin("HTTP server", resolver.NewAPIPlugin(c.resolver))
	}

	// launch third-party plugins
	for _, pconfig := range c.config.Plugins {
		pluginName := pconfig.Name
		if pluginName == "" {
			logging.Error.Printf("failed to register plugin with empty name")
			continue
		}
		plugin, err := plugins.New(pluginName, pconfig.Settings)
		if err != nil {
			logging.Error.Printf("failed to create plugin: %v", err)
			continue
		}
		c.launchPlugin(pluginName, plugin)
	}
}

func (c *app) launchPlugin(pluginName string, plugin plugins.Plugin) {
	logging.Verbose.Printf("starting plugin %q", pluginName)
	pctx := &pluginContext{pluginName: pluginName, app: c}
	if errCh := plugin.Start(pctx); errCh != nil {
		go func() {
			for err := range errCh {
				c.errHandler(pluginName, err)
			}
		}()
	}
	go func() {
		select {
		case <-plugin.Done():
			logging.Verbose.Printf("plugin %q terminated", pluginName)
		}
	}()
}

func launchServer(enabled bool, f func() <-chan error) (errCh <-chan error) {
	if enabled {
		errCh = f()
	}
	return
}

// launch Zookeeper listener
func (c *app) beginLeaderWatch() (newLeader <-chan struct{}, zkErr <-chan error) {
	if c.config.Zk != "" {
		newLeader, zkErr = c.resolver.LaunchZK(zkInitialDetectionTimeout)
	} else {
		// uniform behavior when new leader from masters field
		leader := make(chan struct{}, 1)
		leader <- struct{}{}
		newLeader = leader
	}
	return
}

// periodically reload DNS records, either because the reload timer expired or else
// because a caller invoked the tryReload func returned by this func.
func (c *app) launchReloader() (tryReload func()) {
	// generate reload signal; up to 1 reload pending at any time
	reloadSignal := make(chan struct{}, 1)
	tryReload = func() {
		// non-blocking, attempt to queue a reload
		select {
		case reloadSignal <- struct{}{}:
		default:
		}
	}

	// periodic loading of DNS state (pull from Master)
	go func() {
		defer util.HandleCrash()
		reloadTimeout := time.Second * time.Duration(c.config.RefreshSeconds)
		reloadTimer := time.AfterFunc(reloadTimeout, tryReload)
		for _ = range reloadSignal {
			c.resolver.Reload()
			logging.PrintCurLog()
			reloadTimer.Reset(reloadTimeout)
		}
	}()
	return
}

func (c *app) run() {
	select {
	case <-c.ready:
		select {
		case <-c.done:
			panic("run already completed")
		default:
			defer close(c.done)
		}
	default:
		panic("cannot run, not yet initialized")
	}

	// launch async server procs
	dnsErr := launchServer(c.config.DnsOn, func() <-chan error {
		return c.resolver.LaunchDNS(c.filters.Apply)
	})
	newLeader, zkErr := c.beginLeaderWatch()
	tryReload := c.launchReloader()

	// infinite loop until there is fatal error
	// TODO(jdef) it would be nice to extend error handling to plugins
	for {
		select {
		case <-newLeader:
			tryReload()
		case err := <-dnsErr:
			c.errHandler("DNS server", err)
		case err := <-zkErr:
			c.errHandler("ZK watcher", err)
		}
	}
}