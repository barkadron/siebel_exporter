package main

import (
	"net/http"
	"os"
	"os/signal"
	"syscall"

	"time"

	// "github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	namespace = "siebel" // For Prometheus metrics
	exporterName = namespace + "_exporter"
	defaultPort = "28001"
	defaultEndpoint = "/metrics"
)

var (
	listenAddress = kingpin.Flag("port", "Port to listen on.").Default(defaultPort).String()
	metricsEndpoint  = kingpin.Flag("endpoint", "Path under which to expose metrics.").Default(defaultEndpoint).String()
	gracefulStop = make(chan os.Signal)
)

func main() {
	
	// Parse flags
	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print(exporterName))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	// Listen to termination signals from the OS
	signal.Notify(gracefulStop, syscall.SIGTERM)
	signal.Notify(gracefulStop, syscall.SIGINT)
	signal.Notify(gracefulStop, syscall.SIGHUP)
	signal.Notify(gracefulStop, syscall.SIGQUIT)

	// exporter := NewExporter(*scrapeURI)
	// prometheus.MustRegister(exporter)
	// prometheus.MustRegister(version.NewCollector(exporterName))

	log.Infoln("Starting " + exporterName, version.Info())
	log.Infoln("Build context: ", version.BuildContext())
	log.Infoln("Starting Server on port: ", *listenAddress)

	// listener for the termination signals from the OS
	go func() {
		log.Infof("Listening and wait for graceful stop.")
		sig := <-gracefulStop
		log.Infof("Caught sig: %+v. Wait 2 seconds...", sig)
		time.Sleep(2 * time.Second)
		log.Infof("Terminate %s on port: %s", exporterName, *listenAddress)
		os.Exit(0)
	}()

	http.Handle(*metricsEndpoint, promhttp.Handler())
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>Siebel Exporter</title></head><body><h1>Siebel Exporter</h1><p><a href='` + *metricsEndpoint + `'>Metrics</a></p></body></html>`))
	})

	log.Fatal(http.ListenAndServe(":"+*listenAddress, nil))
}
