package webserver

import (
	"context"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
)

func StartWebServer(ctx context.Context, listenAddress string, telemetryPath string, readTimeout int, writeTimeout int, idleTimeout int, securedMetrics bool, serverCert string, serverKey string) (err error) {

	// Add handlers to routes
	mux := http.NewServeMux()
	mux.Handle(telemetryPath, promhttp.HandlerFor(prometheus.DefaultGatherer,
		promhttp.HandlerOpts{ // See more info on https://github.com/prometheus/client_golang/blob/master/prometheus/promhttp/http.go#L269
			ErrorLog:      log.NewErrorLogger(),
			ErrorHandling: promhttp.ContinueOnError,
		}))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>Siebel Exporter</title></head><body><h1>Siebel Exporter</h1><p><a href='` + telemetryPath + `'>Metrics</a></p></body></html>`))
	})

	httpServer := &http.Server{
		Addr:         listenAddress,
		Handler:      mux,
		ErrorLog:     log.NewErrorLogger(),
		ReadTimeout:  time.Second * time.Duration(readTimeout),
		WriteTimeout: time.Second * time.Duration(writeTimeout),
		IdleTimeout:  time.Second * time.Duration(idleTimeout),
	}

	go func() {
		if securedMetrics {
			if _, err := os.Stat(serverCert); err != nil {
				log.Fatalf("Error loading certificate: '%v'", err)
				// return err
			}
			if _, err := os.Stat(serverKey); err != nil {
				log.Fatalf("Error loading key: '%v'", err)
				// return err
			}

			log.Infoln("Listening TLS server on", httpServer.Addr)
			if err := httpServer.ListenAndServeTLS(serverCert, serverKey); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTP server ListenAndServeTLS: '%v'", err)
				// return err
			}
		} else {
			log.Infoln("Listening server on ", httpServer.Addr)
			if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				log.Fatalf("HTTP server ListenAndServe: '%v'", err)
				// return err
			}
		}
	}()

	log.Infof("server started")

	<-ctx.Done()

	log.Infof("server stopped")

	// https://medium.com/honestbee-tw-engineer/gracefully-shutdown-in-go-http-server-5f5e6b83da5a
	// https://medium.com/over-engineering/graceful-shutdown-with-go-http-servers-and-kubernetes-rolling-updates-6697e7db17cf
	// https://rafallorenz.com/go/handle-signals-to-graceful-shutdown-http-server/
	ctxShutDown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err = httpServer.Shutdown(ctxShutDown); err != nil {
		log.Fatalf("server Shutdown Failed:%+s", err)
	}

	log.Infof("server exited properly")

	if err == http.ErrServerClosed {
		err = nil
	}

	return err
}
