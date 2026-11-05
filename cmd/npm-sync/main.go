// Command npm-sync keeps backup Nginx Proxy Manager instances identical to a
// master instance.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/toyonix/nginx-proxy-manager-sync/internal/config"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/logging"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/scheduler"
	npmsync "github.com/toyonix/nginx-proxy-manager-sync/internal/sync"
	"github.com/toyonix/nginx-proxy-manager-sync/internal/web"
)

// version is set at build time with -ldflags "-X main.version=...".
var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	var (
		configPath  = flag.String("config", "", "path to the config file (default $NPM_SYNC_CONFIG or "+config.DefaultPath+")")
		printConfig = flag.Bool("print-config", false, "print the effective configuration with passwords masked, then exit")
		showVersion = flag.Bool("version", false, "print the version and exit")
		forcePw     = flag.Bool("force-passwords", false, "one-shot mode: send every configured access list password")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println("npm-sync", version)
		return 0
	}

	path, required := *configPath, *configPath != ""
	if !required {
		if p := os.Getenv("NPM_SYNC_CONFIG"); p != "" {
			path, required = p, true
		} else {
			path = config.DefaultPath
		}
	}
	cfg, err := config.Load(path, required, os.Environ())
	if err != nil {
		fmt.Fprintln(os.Stderr, "npm-sync:", err)
		return 2
	}

	if *printConfig {
		out, err := cfg.Masked()
		if err != nil {
			fmt.Fprintln(os.Stderr, "npm-sync:", err)
			return 1
		}
		fmt.Print(out)
		return 0
	}

	logBuf := logging.NewBuffer(1000)
	log := logging.New(os.Stdout, cfg.Log.Format, cfg.Log.Level, logBuf)
	slog.SetDefault(log)

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := cfg.CheckSameInstance(ctx, net.DefaultResolver); err != nil {
		log.Error("refusing to start", "error", err)
		return 2
	}

	log.Info("npm-sync starting", "version", version, "master", cfg.Master.URL, "backups", len(cfg.Backups),
		"cron", cfg.Cron, "runOnStart", cfg.RunOnStart, "dryRun", cfg.DryRun)

	syncer := npmsync.New(cfg, log)
	sched := scheduler.New(cfg.Cron, syncer.Run, log)
	metrics := web.NewMetrics()
	sched.OnResult = metrics.Observe

	// One-shot mode: no schedule and no web server (spec section 5).
	if cfg.Cron == "" && cfg.Web.Port == 0 {
		if !cfg.RunOnStart {
			log.Error("nothing to do: cron is empty, runOnStart is false and the web server is disabled")
			return 2
		}
		log.Info("one-shot mode: running once, then exiting")
		res := sched.RunNow(ctx, "one-shot", npmsync.RunOptions{ForcePasswords: *forcePw})
		if res == nil || !res.Succeeded() {
			return 1
		}
		return 0
	}
	if *forcePw {
		log.Warn("--force-passwords only applies in one-shot mode; use the \"Sync now (send passwords)\" button or POST /api/v1/sync?forcePasswords=true")
	}

	if err := sched.Start(ctx); err != nil {
		log.Error("starting scheduler", "error", err)
		return 2
	}

	var srv *http.Server
	if cfg.Web.Port != 0 {
		srv = &http.Server{
			Addr:              fmt.Sprintf(":%d", cfg.Web.Port),
			Handler:           web.New(cfg, sched, logBuf, metrics, version, log).Handler(),
			ReadHeaderTimeout: 10 * time.Second,
		}
		ln, err := net.Listen("tcp", srv.Addr)
		if err != nil {
			log.Error("starting web server", "error", err)
			return 2
		}
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Error("web server stopped", "error", err)
				stop()
			}
		}()
		auth := "off"
		if cfg.Web.Username != "" || cfg.Web.Password != "" {
			auth = "on"
		}
		log.Info("web UI and API listening", "addr", srv.Addr, "basicAuth", auth, "metrics", cfg.Web.Metrics)
	}

	if cfg.RunOnStart {
		sched.Trigger("start", npmsync.RunOptions{})
	}

	<-ctx.Done()
	log.Info("shutdown signal received, stopping")
	if srv != nil {
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		_ = srv.Shutdown(shutCtx)
		cancel()
	}
	sched.Stop()
	log.Info("npm-sync stopped")
	return 0
}
