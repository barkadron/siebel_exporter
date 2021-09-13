package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/barkadron/siebel_exporter/exporter"
	"github.com/barkadron/siebel_exporter/srvrmgr"
	"github.com/barkadron/siebel_exporter/webserver"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"gopkg.in/alecthomas/kingpin.v2"
)

var (
	srvrmgrConnectCmd = kingpin.Flag("srvrmgr.connect-command", "Command for connect to srvrmgr. (env: SRVRMGR_CONNECT_CMD).").Default(getEnv("SRVRMGR_CONNECT_CMD", "")).String() // Example: "source /siebel/ses/siebsrvr/siebenv.sh && srvrmgr /g localhost /e SBA_82 /s sbldev /u SADMIN /p SADMIN /q"
	readBufferSize    = kingpin.Flag("srvrmgr.read-buffer-size", "Size (in bytes) of buffer for reading command output. (env: SRVRMGR_READ_BUFFER_SIZE).").Default(getEnv("SRVRMGR_READ_BUFFER_SIZE", "1024")).Int()
	dateFormat        = kingpin.Flag("srvrmgr.date-format", "Date format (in GO-style) used by srvrmgr. Default value is equal to 'yyyy-mm-dd HH:MM:SS'. (env: SRVRMGR_DATE_FORMAT).").Default(getEnv("SRVRMGR_DATE_FORMAT", "2006-01-02 15:04:05")).String() // yyyy-mm-dd HH:MM:SS
	// commandTimeout    = kingpin.Flag("srvrmgr.command-timeout", "Maximum duration to wait for command execution. (env: SRVRMGR_COMMAND_TIMEOUT).").Default(getEnv("SRVRMGR_COMMAND_TIMEOUT", "5")).Int()

	defaultMetricsFile     = kingpin.Flag("exporter.default-metrics", "Path to TOML-file with default metrics. (env: EXP_DEFAULT_METRICS).").Default(getEnv("EXP_DEFAULT_METRICS", "default-metrics.toml")).String()
	customMetricsFile      = kingpin.Flag("exporter.custom-metrics", "Path to TOML-file that may contain various custom metrics. (env: EXP_CUSTOM_METRICS).").Default(getEnv("EXP_CUSTOM_METRICS", "")).String()
	overrideEmptyMetrics   = kingpin.Flag("exporter.override-empty-metrics", "Override empty metric values with '0'. (env: EXP_OVERRIDE_EMPTY_METRICS).").Default(getEnv("EXP_OVERRIDE_EMPTY_METRICS", "true")).Bool()
	disableExtendedMetrics = kingpin.Flag("exporter.disable-extended-metrics", "Disable metrics with Extended flag. (env: EXP_DISABLE_EXTENDED_METRICS).").Default(getEnv("EXP_DISABLE_EXTENDED_METRICS", "false")).Bool()

	listenPort          = kingpin.Flag("web.listen-port", "Port to listen on for web interface and metrics. (env: WEB_LISTEN_PORT).").Default(getEnv("WEB_LISTEN_PORT", "9870")).Int()
	metricsEndpoint     = kingpin.Flag("web.metrics-endpoint", "Path under which to expose metrics. (env: WEB_METRICS_ENDPOINT).").Default(getEnv("WEB_METRICS_ENDPOINT", "/metrics")).String()
	httpReadTimeout     = kingpin.Flag("web.http-read-timeout", "Maximum duration for reading the entire request. (env: WEB_HTTP_READ_TIMEOUT).").Default(getEnv("WEB_HTTP_READ_TIMEOUT", "30")).Int()
	httpWriteTimeout    = kingpin.Flag("web.http-write-timeout", "Maximum duration before timing out writes of the response. (env: WEB_HTTP_WRITE_TIMEOUT).").Default(getEnv("WEB_HTTP_WRITE_TIMEOUT", "30")).Int()
	httpIdleTimeout     = kingpin.Flag("web.http-idle-timeout", "Maximum duration to wait for the next request when keep-alives are enabled. (env: WEB_HTTP_IDLE_TIMEOUT).").Default(getEnv("WEB_HTTP_IDLE_TIMEOUT", "60")).Int()
	maxRequestsInFlight = kingpin.Flag("web.http-max-requests-in-flight", "Number of concurrent HTTP-requests. Additional requests are responded to with 503 Service Unavailable. If '0', no limit is applied. (env: WEB_HTTP_MAX_REQUESTS_IN_FLIGHT).").Default(getEnv("WEB_HTTP_MAX_REQUESTS_IN_FLIGHT", "0")).Int()
	shutdownTimeout     = kingpin.Flag("web.shutdown-timeout", "Maximum duration to wait for web-server shutdown before forced terminate. (env: WEB_SHUTDOWN_TIMEOUT).").Default(getEnv("WEB_SHUTDOWN_TIMEOUT", "3")).Int()

	useTLS     = kingpin.Flag("tls.enable", "Expose metrics using https.").Default("false").Bool()
	serverCert = kingpin.Flag("tls.server-cert", "Path to the PEM encoded certificate.").String()
	serverKey  = kingpin.Flag("tls.server-key", "Path to the PEM encoded key.").String()
)

func main() {
	const exporterName = "siebel_exporter"
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

	// Listen to termination signals from the OS
	log.Debugln("Listening for the termination signals from the OS and wait for graceful shutdown...")
	signalCh := make(chan os.Signal, 1)
	signal.Notify(signalCh, os.Interrupt, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		sig := <-signalCh
		log.Infof("Caught signal: '%+v'. Gracefully shutting down...", sig)
		signal.Stop(signalCh)
		close(signalCh)
		webCtxCancel()
	}()

	srvrMgr = srvrmgr.NewSrvrmgr(*srvrmgrConnectCmd, *readBufferSize)
	siebelExporter := exporter.NewExporter(srvrMgr, *defaultMetricsFile, *customMetricsFile, *dateFormat, *overrideEmptyMetrics, *disableExtendedMetrics)

	prometheus.MustRegister(siebelExporter)
	prometheus.MustRegister(version.NewCollector(exporterName))

	terminateSrvrmgr := func(cancel context.CancelFunc) {
		defer cancel()
		log.Info("	- Terminate srvrmgr")
		if srvrMgr != nil {
			srvrMgr.Disconnect()
		}
		log.Infof("	- Close %s", exporterName)
	}

	webserver.StartWebServer(webCtx, *listenPort, *metricsEndpoint, *httpReadTimeout, *httpWriteTimeout, *httpIdleTimeout, *shutdownTimeout, *maxRequestsInFlight, *useTLS, *serverCert, *serverKey, &terminateSrvrmgr)

	os.Exit(0)
}

// getEnv returns the value of an environment variable, or returns the provided fallback value
func getEnv(key, fallback string) string {
	if value, exists := os.LookupEnv(key); exists {
		return value
	}
	return fallback
}
