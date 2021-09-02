package main

import (
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
	listenAddress      = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry. (env: LISTEN_ADDRESS)").Default(getEnv("LISTEN_ADDRESS", ":28001")).String()
	telemetryPath      = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics. (env: TELEMETRY_PATH)").Default(getEnv("TELEMETRY_PATH", "/metrics")).String()
	defaultMetricsFile = kingpin.Flag("default.metrics", "File with default metrics in a TOML file. (env: DEFAULT_METRICS)").Default(getEnv("DEFAULT_METRICS", "default-metrics.toml")).String()
	customMetricsFile  = kingpin.Flag("custom.metrics", "File that may contain various custom metrics in a TOML file. (env: CUSTOM_METRICS)").Default(getEnv("CUSTOM_METRICS", "")).String()

	/*
		siebenvPath = kingpin.Flag("srvrmgr.siebenv-path", "Path to the Siebel environment setup file 'siebenv.sh'. (env: SRVRMGR_SIEBENVSH_PATH)").Default(getEnv("SRVRMGR_SIEBENVSH_PATH", "")).String()
		gateway = kingpin.Flag("srvrmgr.gateway", "Network address of the Siebel Gateway Name Server. (env: SRVRMGR_GATEWAY)").Default(getEnv("SRVRMGR_GATEWAY", "localhost")).String()
		enterpriseName  = kingpin.Flag("srvrmgr.enterprise-name", "Siebel Enterprise Server name. (env: SRVRMGR_ENTERPRISE_NAME)").Default(getEnv("SRVRMGR_ENTERPRISE_NAME", "")).String()
		userName = kingpin.Flag("srvrmgr.user-name", "Siebel Server administrator user name. (env: SRVRMGR_USER_NAME)").Default(getEnv("SRVRMGR_USER_NAME", "SADMIN")).String()
		userPass = kingpin.Flag("srvrmgr.user-pass", "Siebel Server administrator password. (env: SRVRMGR_USER_PASS)").Default(getEnv("SRVRMGR_USER_PASS", "SADMIN")).String()
	*/

	dateFormat     = kingpin.Flag("srvrmgr.date-format", "Date format used by srvrmgr. (env: SRVRMGR_DATE_FORMAT)").Default(getEnv("SRVRMGR_DATE_FORMAT", "2006-01-02 15:04:05")).String() // yyyy-mm-dd HH:MM:SS
	readBufferSize = kingpin.Flag("srvrmgr.read-buffer-size", "Size (in bytes) of buffer for reading srvrmgr commands output. (env: SRVRMGR_READ_BUFFER_SIZE)").Default(getEnv("SRVRMGR_READ_BUFFER_SIZE", "512")).Int()
	// queryTimeout      = kingpin.Flag("srvrmgr.query-timeout", "Query timeout (in seconds). (env: SRVRMGR_QUERY_TIMEOUT)").Default(getEnv("SRVRMGR_QUERY_TIMEOUT", "5")).Int()
	srvrmgrConnectCmd = kingpin.Flag("srvrmgr.connect-command", "Command for connect to srvrmgr. (env: SRVRMGR_CONNECT_CMD)").Default(getEnv("SRVRMGR_CONNECT_CMD", "")).String()
	// Example: "source /siebel/ses/siebsrvr/siebenv.sh && srvrmgr /g localhost /e SBA_82 /s sbldev /u SADMIN /p SADMIN /q"

	overrideEmptyMetricsValue = kingpin.Flag("exporter.override-empty-metrics", "Value for override empty metrics.").Default("0").String()

	securedMetrics = kingpin.Flag("web.secured-metrics", "Expose metrics using https.").Default("false").Bool()
	serverCert     = kingpin.Flag("web.ssl-server-cert", "Path to the PEM encoded certificate").ExistingFile()
	serverKey      = kingpin.Flag("web.ssl-server-key", "Path to the PEM encoded key").ExistingFile()
)

func main() {
	var srvrMgr srvrmgr.SrvrMgr = nil
	var webServer webserver.WebServer = nil

	// Parse flags
	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print(exporterName))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	// Version will set during build time
	log.Infoln("Starting "+exporterName, version.Info())
	log.Infoln("Build context: ", version.BuildContext())

	// Channel for graceful shutdown
	gracefulShutdownCh := make(chan os.Signal)

	// Listen to termination signals from the OS
	signal.Notify(gracefulShutdownCh, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		log.Debugln("Listening for the termination signals from the OS and wait for graceful shutdown.")

		sig := <-gracefulShutdownCh
		log.Infof("Caught sig: '%+v'. Gracefully shutting down...", sig)

		signal.Stop(gracefulShutdownCh)
		close(gracefulShutdownCh)

		log.Infof("	- Terminate http(s) server on port %s", *listenAddress)
		if webServer != nil && webServer.IsStarted() {
			webServer.Stop()
		}

		log.Infof("	- Terminate srvrmgr")
		if srvrMgr != nil && srvrMgr.IsConnected() {
			srvrMgr.Disconnect()
		}

		log.Infof("	- Close %s", exporterName)
		os.Exit(0)
	}()

	srvrMgr = srvrmgr.NewSrvrmgr(*srvrmgrConnectCmd, shell.NewShell(*readBufferSize))
	exp := exporter.NewExporter(namespace, subsystem, *dateFormat, *overrideEmptyMetricsValue, *defaultMetricsFile, *customMetricsFile, srvrMgr)
	webServer = webserver.NewWebServer(*listenAddress, *telemetryPath, *securedMetrics, *serverCert, *serverKey)

	prometheus.MustRegister(exp)
	prometheus.MustRegister(version.NewCollector(exporterName))

	if err := webServer.Start(); err != nil {
		panic(err)
	}
}

// getEnv returns the value of an environment variable, or returns the provided fallback value
func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}
