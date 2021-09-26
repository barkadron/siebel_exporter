package webserver

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
)

// StartWebServer create new http-server and start listening on provided port.
func StartWebServer(ctx context.Context, listenPort int, metricsEndpoint string, readTimeout, writeTimeout, idleTimeout, shutdownTimeout, maxRequestsInFlight int, useTLS bool, serverCert, serverKey string, onShutdown *func(cancelShutdown context.CancelFunc)) {
	log.Debugln("StartWebServer")

	if useTLS {
		if _, err := os.Stat(serverCert); err != nil {
			log.Errorf("Error loading certificate: '%v'", err)
			panic(err)
		}
		if _, err := os.Stat(serverKey); err != nil {
			log.Errorf("Error loading key: '%v'", err)
			panic(err)
		}
	}

	router := http.NewServeMux()
	router.Handle(metricsEndpoint, promhttp.HandlerFor(prometheus.DefaultGatherer,
		promhttp.HandlerOpts{ // See more info on https://github.com/prometheus/client_golang/blob/master/prometheus/promhttp/http.go#L269
			ErrorLog:            log.NewErrorLogger(),
			ErrorHandling:       promhttp.ContinueOnError,
			Registry:            prometheus.DefaultRegisterer,
			MaxRequestsInFlight: maxRequestsInFlight,
		}))
	router.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>Siebel Exporter</title></head><body><h1>Siebel Exporter ` + version.Version + `</h1><p><a href='` + metricsEndpoint + `'>Metrics</a></p></body></html>`))
	})

	httpServer := &http.Server{
		Addr:         ":" + fmt.Sprint(listenPort),
		Handler:      router,
		ErrorLog:     log.NewErrorLogger(),
		ReadTimeout:  time.Second * time.Duration(readTimeout),
		WriteTimeout: time.Second * time.Duration(writeTimeout),
		IdleTimeout:  time.Second * time.Duration(idleTimeout),
	}

	go func() {
		if useTLS {
			if err := httpServer.ListenAndServeTLS(serverCert, serverKey); err != http.ErrServerClosed {
				log.Errorf("HTTP server ListenAndServeTLS: '%v'", err)
			}
		} else {
			if err := httpServer.ListenAndServe(); err != http.ErrServerClosed {
				log.Errorf("HTTP server ListenAndServe: '%v'", err)
			}
		}
	}()

	log.Infoln("Web-server listening on", httpServer.Addr)

	<-ctx.Done()

	log.Debugln("Web-server received signal to shutdown.")

	// https://medium.com/honestbee-tw-engineer/gracefully-shutdown-in-go-http-server-5f5e6b83da5a
	// https://medium.com/over-engineering/graceful-shutdown-with-go-http-servers-and-kubernetes-rolling-updates-6697e7db17cf
	// https://rafallorenz.com/go/handle-signals-to-graceful-shutdown-http-server/
	ctxShutdown, cancelShutdown := context.WithTimeout(context.Background(), time.Second*time.Duration(shutdownTimeout))

	idleConnClosedCh := make(chan bool, 1)

	httpServer.RegisterOnShutdown(func() {
		log.Debugln("RegisterOnShutdown")
		// Waiting for web-server close idle connections and then terminate srvrmgr
		<-idleConnClosedCh
		(*onShutdown)(cancelShutdown)
	})

	log.Info("	- Terminate web-server")
	if err := httpServer.Shutdown(ctxShutdown); err != nil {
		log.Errorf("Web-server shutdown error: %+s", err)
	} else {
		log.Debugln("Web-server gracefully stopped.")
	}

	idleConnClosedCh <- true

	// waiting for RegisterOnShutdown or timeout
	<-ctxShutdown.Done()
}
