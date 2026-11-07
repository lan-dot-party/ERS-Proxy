package cmd

import (
	"context"
	"errors"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/spf13/cobra"

	"github.com/lan-dot-party/ers-proxy/internal/app"
	"github.com/lan-dot-party/ers-proxy/internal/health"
	"github.com/lan-dot-party/ers-proxy/internal/monitor"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/drivers"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/history"
	"github.com/lan-dot-party/ers-proxy/internal/monitor/poller"
	"github.com/lan-dot-party/ers-proxy/internal/web"
	"github.com/lan-dot-party/ers-proxy/pkg/version"
)

func init() {
	rootCmd.AddCommand(serverCmd)
}

var serverCmd = &cobra.Command{
	Use:   "server",
	Short: "Run the web UI and the EDM proxy",
	RunE: func(cmd *cobra.Command, args []string) error {
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		e, err := open(ctx)
		if err != nil {
			return err
		}
		defer e.close()
		log := e.log
		log.Info("starting ers-proxy", "version", version.GetVersion(), "listen", e.cfg.Listen, "public_url", e.cfg.PublicURL)
		if !e.cfg.SecureCookies() && e.cfg.TLS.CertFile == "" {
			log.Warn("public_url is not https: passwords and switch data travel unencrypted between browser and ERS-Proxy")
		}

		a := app.New(e.cfg, e.db, e.key, log)
		ui, err := web.New(a)
		if err != nil {
			return err
		}
		ui.UpdateFrameSources(ctx)
		proxy, err := newProxy(ctx, a, ui)
		if err != nil {
			return err
		}
		checker := health.New(e.db, e.cfg, log)
		ui.Health = checker
		mon := poller.New(e.db, e.key, monitor.NewRegistry(drivers.All()...), log)
		edm := newEDMSource(proxy, e.key)
		mon.OpenEDM = edm.open
		mon.OpenSwOS = newSwOSSource(proxy)
		mon.OpenGWN = newGWNSource(proxy, e.key).open
		rec := history.New(e.db, e.cfg.Monitor.Retention, log)
		mon.Sink = rec
		ui.Monitor = mon
		ui.History = rec
		forget := ui.OnDeviceChange
		ui.OnDeviceChange = func(id int64) {
			if forget != nil {
				forget(id)
			}
			checker.Trigger()
			mon.Trigger()
		}
		if e.cfg.Configure.Enabled {
			// Only config.yaml turns this on (configure.enabled).
			log.Warn("changing switch settings is enabled (configure.enabled): admins can write to switches with a configuration channel")
			mgr := newConfigure(e, edm, mon)
			ui.Configure = mgr
			go mgr.Run(ctx)
		}
		go checker.Run(ctx)
		go mon.Run(ctx)
		go rec.Run(ctx)

		r := chi.NewRouter()
		r.Use(middleware.Recoverer, a.RequestLog, middleware.GetHead)
		r.Get("/health", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain")
			if err := e.db.Ping(r.Context()); err != nil {
				http.Error(w, "db unavailable", http.StatusServiceUnavailable)
				return
			}
			_, _ = w.Write([]byte("ok"))
		})
		// Proxied device content: own security headers, no portal session.
		r.Handle("/d/*", proxy)
		r.With(a.SecurityHeaders, a.SessionMiddleware).Mount("/", ui.Routes())

		srv := &http.Server{
			Addr:              e.cfg.Listen,
			Handler:           r,
			ReadHeaderTimeout: 10 * time.Second,
			ReadTimeout:       e.cfg.Proxy.RequestTimeout + 10*time.Second,
			WriteTimeout:      e.cfg.Proxy.RequestTimeout + 30*time.Second,
			IdleTimeout:       120 * time.Second,
			MaxHeaderBytes:    64 << 10,
		}
		errc := make(chan error, 1)
		go func() {
			if e.cfg.TLS.CertFile != "" {
				errc <- srv.ListenAndServeTLS(e.cfg.ResolvePath(e.cfg.TLS.CertFile), e.cfg.ResolvePath(e.cfg.TLS.KeyFile))
			} else {
				errc <- srv.ListenAndServe()
			}
		}()
		log.Info("http listening", "addr", e.cfg.Listen)
		select {
		case err := <-errc:
			if !errors.Is(err, http.ErrServerClosed) {
				return err
			}
		case <-ctx.Done():
		}
		log.Info("shutting down")
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	},
}
