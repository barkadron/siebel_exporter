package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/barkadron/siebel_exporter/exporter"
	"github.com/barkadron/siebel_exporter/shell"
	"github.com/barkadron/siebel_exporter/srvrmgr"
	"github.com/barkadron/siebel_exporter/webserver"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	namespace    = "siebel"
	subsystem    = "exporter"
	exporterName = namespace + "_" + subsystem
)

var (
	listenAddress    = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry. (env: LISTEN_ADDRESS)").Default(getEnv("LISTEN_ADDRESS", ":28001")).String()
	telemetryPath    = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics. (env: TELEMETRY_PATH)").Default(getEnv("TELEMETRY_PATH", "/metrics")).String()
	httpReadTimeout  = kingpin.Flag("web.http-read-timeout", "Maximum duration for reading the entire request. (env: WEB_HTTP_READ_TIMEOUT)").Default(getEnv("WEB_HTTP_READ_TIMEOUT", "30")).Int()
	httpWriteTimeout = kingpin.Flag("web.http-write-timeout", "Maximum duration before timing out writes of the response. (env: WEB_HTTP_WRITE_TIMEOUT)").Default(getEnv("WEB_HTTP_WRITE_TIMEOUT", "30")).Int()
	httpIdleTimeout  = kingpin.Flag("web.http-idle-timeout", "Maximum amount of time to wait for the next request when keep-alives are enabled. (env: WEB_HTTP_IDLE_TIMEOUT)").Default(getEnv("WEB_HTTP_IDLE_TIMEOUT", "60")).Int()
	securedMetrics   = kingpin.Flag("web.secured-metrics", "Expose metrics using https.").Default("false").Bool()
	serverCert       = kingpin.Flag("web.ssl-server-cert", "Path to the PEM encoded certificate").ExistingFile()
	serverKey        = kingpin.Flag("web.ssl-server-key", "Path to the PEM encoded key").ExistingFile()

	defaultMetricsFile   = kingpin.Flag("default.metrics", "File with default metrics in a TOML file. (env: DEFAULT_METRICS)").Default(getEnv("DEFAULT_METRICS", "default-metrics.toml")).String()
	customMetricsFile    = kingpin.Flag("custom.metrics", "File that may contain various custom metrics in a TOML file. (env: CUSTOM_METRICS)").Default(getEnv("CUSTOM_METRICS", "")).String()
	emptyMetricsOverride = kingpin.Flag("exporter.empty-metrics-override", "Value for override empty metrics.").Default("0").String()

	dateFormat        = kingpin.Flag("srvrmgr.date-format", "Date format used by srvrmgr. (env: SRVRMGR_DATE_FORMAT)").Default(getEnv("SRVRMGR_DATE_FORMAT", "2006-01-02 15:04:05")).String() // yyyy-mm-dd HH:MM:SS
	readBufferSize    = kingpin.Flag("srvrmgr.read-buffer-size", "Size (in bytes) of buffer for reading srvrmgr commands output. (env: SRVRMGR_READ_BUFFER_SIZE)").Default(getEnv("SRVRMGR_READ_BUFFER_SIZE", "512")).Int()
	srvrmgrConnectCmd = kingpin.Flag("srvrmgr.connect-command", "Command for connect to srvrmgr. (env: SRVRMGR_CONNECT_CMD)").Default(getEnv("SRVRMGR_CONNECT_CMD", "")).String() // Example: "source /siebel/ses/siebsrvr/siebenv.sh && srvrmgr /g localhost /e SBA_82 /s sbldev /u SADMIN /p SADMIN /q"
	// queryTimeout      = kingpin.Flag("srvrmgr.query-timeout", "Query timeout (in seconds). (env: SRVRMGR_QUERY_TIMEOUT)").Default(getEnv("SRVRMGR_QUERY_TIMEOUT", "5")).Int()
)

func main() {
	var srvrMgr srvrmgr.SrvrMgr = nil

	// Parse flags
	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print(exporterName))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	// Version will set during build time
	log.Infoln("Starting "+exporterName, version.Info())
	log.Infoln("Build context: ", version.BuildContext())

	// Context for shutdown web-server
	webCtx, webCtxCancel := context.WithCancel(context.Background())

	// Channel for graceful shutdown
	signalCh := make(chan os.Signal, 1)

	// Listen to termination signals from the OS
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		log.Debugln("Listening for the termination signals from the OS and wait for graceful shutdown.")

		sig := <-signalCh
		log.Infof("Caught sig: '%+v'. Gracefully shutting down...", sig)

		signal.Stop(signalCh)
		close(signalCh)

		log.Info("	- Terminate web-server")
		webCtxCancel()

		log.Info("	- Terminate srvrmgr")
		if srvrMgr != nil && srvrMgr.IsConnected() {
			if err := srvrMgr.Disconnect(); err != nil {
				log.Errorf("Error on srvrmgr disconnect: %v", err)
			}
		}

		log.Infof("	- Close %s", exporterName)
		os.Exit(0)
	}()

	srvrMgr = srvrmgr.NewSrvrmgr(*srvrmgrConnectCmd, shell.NewShell(*readBufferSize))
	exp := exporter.NewExporter(namespace, subsystem, *defaultMetricsFile, *customMetricsFile, *dateFormat, *emptyMetricsOverride, srvrMgr)

	prometheus.MustRegister(exp)
	prometheus.MustRegister(version.NewCollector(exporterName))

	if err := webserver.StartWebServer(webCtx, *listenAddress, *telemetryPath, *httpReadTimeout, *httpWriteTimeout, *httpIdleTimeout, *securedMetrics, *serverCert, *serverKey); err != nil {
		log.Errorf("failed to serve:+%v\n", err)
	}
}

// getEnv returns the value of an environment variable, or returns the provided fallback value
func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
