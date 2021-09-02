package webserver

import (
	"net/http"
	"os"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
)

type webServer struct {
	started        bool
	listenAddress  string
	telemetryPath  string
	securedMetrics bool
	serverCert     string
	serverKey      string
}

type WebServer interface {
	Start() error
	Stop() error
	IsStarted() bool
}

func NewWebServer(listenAddress string, telemetryPath string, securedMetrics bool, serverCert string, serverKey string) WebServer {
	ws := &webServer{
		started:        false,
		listenAddress:  listenAddress,
		telemetryPath:  telemetryPath,
		securedMetrics: securedMetrics,
		serverCert:     serverCert,
		serverKey:      serverKey,
	}

	return ws
}

func (ws *webServer) IsStarted() bool {
	return ws.started
}

func (ws *webServer) Stop() error {
	// @TODO: need to gracefully stop http(s) server
	// https://medium.com/honestbee-tw-engineer/gracefully-shutdown-in-go-http-server-5f5e6b83da5a
	// https://medium.com/over-engineering/graceful-shutdown-with-go-http-servers-and-kubernetes-rolling-updates-6697e7db17cf
	time.Sleep(100 * time.Millisecond)
	return nil
}

func (ws *webServer) Start() error {
	// See more info on https://github.com/prometheus/client_golang/blob/master/prometheus/promhttp/http.go#L269
	opts := promhttp.HandlerOpts{
		ErrorLog:      log.NewErrorLogger(),
		ErrorHandling: promhttp.ContinueOnError,
	}
	http.Handle(ws.telemetryPath, promhttp.HandlerFor(prometheus.DefaultGatherer, opts))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>Siebel Exporter</title></head><body><h1>Siebel Exporter</h1><p><a href='` + ws.telemetryPath + `'>Metrics</a></p></body></html>`))
	})

	if ws.securedMetrics {
		if _, err := os.Stat(ws.serverCert); err != nil {
			log.Fatal("Error loading certificate:", err)
			return err
		}
		if _, err := os.Stat(ws.serverKey); err != nil {
			log.Fatal("Error loading key:", err)
			return err
		}
		log.Infoln("Listening TLS server on", ws.listenAddress)
		if err := http.ListenAndServeTLS(ws.listenAddress, ws.serverCert, ws.serverKey, nil); err != nil {
			log.Fatal("Failed to start the secure server:", err)
			return err
		}
	} else {
		log.Infoln("Listening server on", ws.listenAddress)
		if err := http.ListenAndServe(ws.listenAddress, nil); err != nil {
			log.Fatal("Failed to start the server:", err)
			return err
		}
	}
	return nil
}
