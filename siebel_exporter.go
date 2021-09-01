package main

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
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

	gracefulShutdown = make(chan os.Signal)
)

func main() {
	// Parse flags
	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print(exporterName))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	// Version will set during build time
	log.Infoln("Starting "+exporterName, version.Info())
	log.Infoln("Build context: ", version.BuildContext())

	// Load default and custom metrics
	hashMap = make(map[int][]byte)
	reloadMetrics()

	srvrmgr := NewSrvrmgr(*srvrmgrConnectCmd)

	// Listen to termination signals from the OS
	signal.Notify(gracefulShutdown, syscall.SIGTERM, syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)
	go func() {
		log.Debugln("Listening for the termination signals from the OS and wait for graceful shutdown.")
		sig := <-gracefulShutdown
		log.Infof("Caught sig: %+v. Gracefully shutting down...", sig)

		log.Infof("	- Terminate http(s) server on port %s", *listenAddress)
		// @TODO: need to gracefully stop http(s) server
		// https://medium.com/honestbee-tw-engineer/gracefully-shutdown-in-go-http-server-5f5e6b83da5a
		// https://medium.com/over-engineering/graceful-shutdown-with-go-http-servers-and-kubernetes-rolling-updates-6697e7db17cf
		time.Sleep(100 * time.Millisecond)

		log.Infof("	- Terminate srvrmgr")
		srvrmgr.Disconnect()

		log.Infof("	- Close %s", exporterName)
		os.Exit(0)
	}()

	exporter := NewExporter(srvrmgr)
	prometheus.MustRegister(exporter)
	prometheus.MustRegister(version.NewCollector(exporterName))

	// See more info on https://github.com/prometheus/client_golang/blob/master/prometheus/promhttp/http.go#L269
	opts := promhttp.HandlerOpts{
		ErrorLog:      log.NewErrorLogger(),
		ErrorHandling: promhttp.ContinueOnError,
	}
	http.Handle(*telemetryPath, promhttp.HandlerFor(prometheus.DefaultGatherer, opts))
	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`<html><head><title>Siebel Exporter</title></head><body><h1>Siebel Exporter</h1><p><a href='` + *telemetryPath + `'>Metrics</a></p></body></html>`))
	})

	if *securedMetrics {
		if _, err := os.Stat(*serverCert); err != nil {
			log.Fatal("Error loading certificate:", err)
			panic(err)
		}
		if _, err := os.Stat(*serverKey); err != nil {
			log.Fatal("Error loading key:", err)
			panic(err)
		}
		log.Infoln("Listening TLS server on", *listenAddress)
		// log.Infof("You can check the metrics at https://localhost%s%s", *listenAddress, *telemetryPath)
		if err := http.ListenAndServeTLS(*listenAddress, *serverCert, *serverKey, nil); err != nil {
			log.Fatal("Failed to start the secure server:", err)
			panic(err)
		}
	} else {
		log.Infoln("Listening server on", *listenAddress)
		// log.Infof("You can check the metrics at http://localhost%s%s", *listenAddress, *telemetryPath)
		if err := http.ListenAndServe(*listenAddress, nil); err != nil {
			log.Fatal("Failed to start the server:", err)
			panic(err)
		}
	}
}

// getEnv returns the value of an environment variable, or returns the provided fallback value
func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// Metrics object description
type Metric struct {
	Context          string
	Labels           []string
	MetricsDesc      map[string]string
	MetricsType      map[string]string
	MetricsBuckets   map[string]map[string]string
	FieldToAppend    string
	Request          string
	IgnoreZeroResult bool
}

// Used to load multiple metrics from file
type Metrics struct {
	Metric []Metric
}

// Metrics to scrap. Use external file (default-metrics.toml and custom if provided)
var (
	metricsToScrap    Metrics
	additionalMetrics Metrics
	hashMap           map[int][]byte
)

// Exporter collects Siebel metrics. It implements prometheus.Collector.
type Exporter struct {
	srvrmgr         iSRVRMGR
	duration, error prometheus.Gauge
	totalScrapes    prometheus.Counter
	scrapeErrors    *prometheus.CounterVec
	gatewayServerUp prometheus.Gauge
	// appServerUp     prometheus.Gauge
}

// NewExporter returns a new Siebel exporter for the provided args.
func NewExporter(srvrmgr iSRVRMGR) *Exporter {
	// log.Infoln("Creating new Exporter...")

	// maybe this is superfluous...
	// labels := map[string]string{
	// 	"server_name": srvrmgr.getApplicationServerName(),
	// }

	return &Exporter{
		srvrmgr: srvrmgr,
		duration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "last_scrape_duration_seconds",
			Help:      "Duration of the last scrape of metrics from Siebel.",
			// ConstLabels: labels,
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "scrapes_total",
			Help:      "Total number of times Siebel was scraped for metrics.",
			// ConstLabels: labels,
		}),
		scrapeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "scrape_errors_total",
			Help:      "Total number of times an error occured scraping a Siebel.",
			// ConstLabels: labels,
		}, []string{"collector"}),
		error: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "last_scrape_error",
			Help:      "Whether the last scrape of metrics from Siebel resulted in an error (1 for error, 0 for success).",
			// ConstLabels: labels,
		}),
		gatewayServerUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "gateway_server_up",
			Help:      "Whether the Siebel Gateway Server is up (1 for up, 0 for down).",
			// ConstLabels: labels,
		}),
		// appServerUp: prometheus.NewGauge(prometheus.GaugeOpts{
		// 	Namespace:   namespace,
		// 	Name:        "application_server_up",
		// 	Help:        "Whether the Siebel Application Server is up (1 for up, 0 for down).",
		// 	ConstLabels: labels,
		// }),
	}
}

// Describe describes all the metrics exported by the Siebel exporter.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	// We cannot know in advance what metrics the exporter will generate So we use the poor man's describe method: Run a collect
	// and send the descriptors of all the collected metrics. The problem here is that we need to connect to the Siebel. If it is currently
	// unavailable, the descriptors will be incomplete. Since this is a stand-alone exporter and not used as a library within other code
	// implementing additional metrics, the worst that can happen is that we don't detect inconsistent metrics created by this exporter
	// itself. Also, a change in the monitored Siebel instance may change the exported metrics during the runtime of the exporter.

	log.Debugln("Exporter.Describe")

	metricCh := make(chan prometheus.Metric)
	doneCh := make(chan struct{})

	go func() {
		for m := range metricCh {
			ch <- m.Desc()
		}
		close(doneCh)
	}()

	e.Collect(metricCh)
	close(metricCh)
	<-doneCh
}

// Collect implements prometheus.Collector.
func (e *Exporter) Collect(ch chan<- prometheus.Metric) {
	e.scrape(ch)
	ch <- e.duration
	ch <- e.totalScrapes
	ch <- e.error
	e.scrapeErrors.Collect(ch)
	ch <- e.gatewayServerUp
	// ch <- e.appServerUp
}

func (e *Exporter) scrape(ch chan<- prometheus.Metric) {
	log.Debugln("Exporter.scrape")

	e.totalScrapes.Inc()
	var err error
	defer func(begun time.Time) {
		e.duration.Set(time.Since(begun).Seconds())
		if err == nil {
			e.error.Set(0)
		} else {
			e.error.Set(1)
		}
	}(time.Now())

	// If not connected, then trying to reconnect
	if !e.srvrmgr.IsConnected() {
		if err = e.srvrmgr.Reconnect(); err != nil {
			// log.Errorln(err)
			e.gatewayServerUp.Set(0)
			return
		} else {
			e.gatewayServerUp.Set(1)
		}
	} else {
		// If already connected, then detecting Siebel Gateway Server is up or down
		if gatewayServerState := e.srvrmgr.PingGatewayServer(); !gatewayServerState {
			log.Warnln("Connection to the Siebel Gateway Server was lost. Will try to reconnect on next scrape.")
			e.gatewayServerUp.Set(0)
			return
		}
		e.gatewayServerUp.Set(1)
	}

	/*
		// Detecting Siebel Application Server is up or down
		if appServerState := e.srvrmgr.pingApplicationServer(); !appServerState {
			log.Warnln("Application Server is down.")
			e.appServerUp.Set(0)
			return
		}
		e.appServerUp.Set(1)
	*/

	if checkIfMetricsChanged() {
		log.Infoln("Metrics changed, reload it.")
		reloadMetrics()
	}

	for _, metric := range metricsToScrap.Metric {
		log.Debugln("About to scrape metric: ")
		log.Debugln("	- Metric MetricsDesc: ", metric.MetricsDesc)
		log.Debugln("	- Metric Context: ", metric.Context)
		log.Debugln("	- Metric MetricsType: ", metric.MetricsType)
		log.Debugln("	- Metric MetricsBuckets: ", metric.MetricsBuckets, "(Ignored unless Histogram type)")
		log.Debugln("	- Metric Labels: ", metric.Labels)
		log.Debugln("	- Metric FieldToAppend: ", metric.FieldToAppend)
		log.Debugln("	- Metric IgnoreZeroResult: ", metric.IgnoreZeroResult)
		log.Debugln("	- Metric Request: ", metric.Request)

		if len(metric.Request) == 0 {
			log.Errorln("Error scraping for ", metric.MetricsDesc, ". Did you forget to define request in your toml file?")
			return
		}

		if len(metric.MetricsDesc) == 0 {
			log.Errorln("Error scraping for request", metric.Request, ". Did you forget to define metricsdesc in your toml file?")
			return
		}

		for column, metricType := range metric.MetricsType {
			if metricType == "histogram" {
				_, ok := metric.MetricsBuckets[column]
				if !ok {
					log.Errorln("Unable to find MetricsBuckets configuration key for metric. (metric=" + column + ")")
					return
				}
			}
		}

		scrapeStart := time.Now()
		if err = ScrapeMetric(e.srvrmgr, ch, metric); err != nil {
			log.Errorln("Error scraping for", metric.Context, "_", metric.MetricsDesc, ":", err)
			e.scrapeErrors.WithLabelValues(metric.Context).Inc()
		} else {
			log.Debugln("Successfully scraped metric: ", metric.Context, metric.MetricsDesc, time.Since(scrapeStart))
		}
	}
}

func GetMetricType(metricType string, metricsType map[string]string) prometheus.ValueType {
	var strToPromType = map[string]prometheus.ValueType{
		"gauge":     prometheus.GaugeValue,
		"counter":   prometheus.CounterValue,
		"histogram": prometheus.UntypedValue,
	}

	strType, ok := metricsType[strings.ToLower(metricType)]
	if !ok {
		return prometheus.GaugeValue
	}
	valueType, ok := strToPromType[strings.ToLower(strType)]
	if !ok {
		panic(errors.New("Error while getting prometheus type " + strings.ToLower(strType)))
	}
	return valueType
}

// interface method to call ScrapeGenericValues using Metric struct values
func ScrapeMetric(srvrmgr iSRVRMGR, ch chan<- prometheus.Metric, metricDefinition Metric) error {
	return ScrapeGenericValues(srvrmgr, ch, metricDefinition.Context, metricDefinition.Labels,
		metricDefinition.MetricsDesc, metricDefinition.MetricsType, metricDefinition.MetricsBuckets,
		metricDefinition.FieldToAppend, metricDefinition.IgnoreZeroResult,
		metricDefinition.Request)
}

// generic method for retrieving metrics.
func ScrapeGenericValues(srvrmgr iSRVRMGR, ch chan<- prometheus.Metric, context string, labels []string,
	metricsDesc map[string]string, metricsType map[string]string, metricsBuckets map[string]map[string]string, fieldToAppend string, ignoreZeroResult bool, request string) error {
	metricsCount := 0
	genericParser := func(row map[string]string) error {
		log.Debugln("genericParser | row map:\n", row)
		// Construct labels value
		labelsValues := []string{}
		for _, label := range labels {
			labelsValues = append(labelsValues, row[label])
		}
		// Construct Prometheus values to sent back
		for metric, metricHelp := range metricsDesc {
			value, err := strconv.ParseFloat(strings.TrimSpace(row[metric]), 64)
			// If not a float, skip current metric
			if err != nil {
				log.Errorln("Unable to convert current value to float (metric=" + metric + ",metricHelp=" + metricHelp + ",value=<" + row[metric] + ">)")
				continue
			}
			log.Debugln("Request result looks like: [", metric, "] : ", value)
			// If metric do not use a field content in metric's name
			if strings.Compare(fieldToAppend, "") == 0 {
				desc := prometheus.NewDesc(
					prometheus.BuildFQName(namespace, context, metric),
					metricHelp,
					labels, nil,
				)
				if metricsType[strings.ToLower(metric)] == "histogram" {
					count, err := strconv.ParseUint(strings.TrimSpace(row["count"]), 10, 64)
					if err != nil {
						log.Errorln("Unable to convert count value to int (metric=" + metric + ",metricHelp=" + metricHelp + ",value=<" + row["count"] + ">)")
						continue
					}
					buckets := make(map[float64]uint64)
					for field, le := range metricsBuckets[metric] {
						lelimit, err := strconv.ParseFloat(strings.TrimSpace(le), 64)
						if err != nil {
							log.Errorln("Unable to convert bucket limit value to float (metric=" + metric + ",metricHelp=" + metricHelp + ",bucketlimit=<" + le + ">)")
							continue
						}
						counter, err := strconv.ParseUint(strings.TrimSpace(row[field]), 10, 64)
						if err != nil {
							log.Errorln("Unable to convert ", field, " value to int (metric="+metric+",metricHelp="+metricHelp+",value=<"+row[field]+">)")
							continue
						}
						buckets[lelimit] = counter
					}
					ch <- prometheus.MustNewConstHistogram(desc, count, value, buckets, labelsValues...)
				} else {
					ch <- prometheus.MustNewConstMetric(desc, GetMetricType(metric, metricsType), value, labelsValues...)
				}
				// If no labels, use metric name
			} else {
				desc := prometheus.NewDesc(
					prometheus.BuildFQName(namespace, context, cleanName(row[fieldToAppend])),
					metricHelp,
					nil, nil,
				)
				if metricsType[strings.ToLower(metric)] == "histogram" {
					count, err := strconv.ParseUint(strings.TrimSpace(row["count"]), 10, 64)
					if err != nil {
						log.Errorln("Unable to convert count value to int (metric=" + metric + ",metricHelp=" + metricHelp + ",value=<" + row["count"] + ">)")
						continue
					}
					buckets := make(map[float64]uint64)
					for field, le := range metricsBuckets[metric] {
						lelimit, err := strconv.ParseFloat(strings.TrimSpace(le), 64)
						if err != nil {
							log.Errorln("Unable to convert bucket limit value to float (metric=" + metric + ",metricHelp=" + metricHelp + ",bucketlimit=<" + le + ">)")
							continue
						}
						counter, err := strconv.ParseUint(strings.TrimSpace(row[field]), 10, 64)
						if err != nil {
							log.Errorln("Unable to convert ", field, " value to int (metric="+metric+",metricHelp="+metricHelp+",value=<"+row[field]+">)")
							continue
						}
						buckets[lelimit] = counter
					}
					ch <- prometheus.MustNewConstHistogram(desc, count, value, buckets)
				} else {
					ch <- prometheus.MustNewConstMetric(desc, GetMetricType(metric, metricsType), value)
				}
			}
			metricsCount++
		}
		return nil
	}
	err := GeneratePrometheusMetrics(srvrmgr, genericParser, request)
	log.Debugln("ScrapeGenericValues() - metricsCount: ", metricsCount)
	if err != nil {
		return err
	}
	if !ignoreZeroResult && metricsCount == 0 {
		return errors.New("no metrics found while parsing")
	}
	return err
}

// Parse srvrmgr result and call parsing function to each row
func GeneratePrometheusMetrics(srvrmgr iSRVRMGR, parse func(row map[string]string) error, request string) error {
	commandResult, err := srvrmgr.ExecuteCommand(request)

	if err != nil {
		return err
	}

	lines := strings.Split(commandResult, "\n")
	if len(lines) < 3 {
		return errors.New("ERROR! Command output is not valid: '" + commandResult + "'")
	}

	columnsRow := lines[0]
	separatorsRow := lines[1]
	dataRows := lines[2:]

	// Get column names
	columns := strings.Split(trimHeadRow(columnsRow), " ")

	// Get column max lengths (calc from separator length)
	spacerLength := getSpacerLength(separatorsRow)
	separators := strings.Split(trimHeadRow(separatorsRow), " ")
	lengths := make([]int, len(separators))
	for i, s := range separators {
		lengths[i] = len(s) + spacerLength
	}

	// Parse data-rows
	log.Debugln("Parsing rows with data...")
	// rowCount := len(dataRows)
	// for i, row := range dataRows {
	for _, row := range dataRows {
		log.Debugln(" - row: '" + row + "'")

		dataMap := make(map[string]string)
		rowLen := len(row)
		for j, colName := range columns {
			colMaxLen := lengths[j]
			if colMaxLen > rowLen {
				colMaxLen = rowLen
			}
			val := strings.TrimSpace(row[:colMaxLen])

			// If value is empty then set it to default "0"
			if len(val) == 0 && len(*overrideEmptyMetricsValue) > 0 {
				val = *overrideEmptyMetricsValue
			}

			// Try to convert date-string to Unix timestamp
			if len(val) == len(*dateFormat) {
				val = convertDateStringToTimestamp(val)
			}

			dataMap[strings.ToLower(colName)] = val

			// Cut off used value from row
			row = row[colMaxLen:]
			rowLen = len(row)
		}

		// Call parser function to parse datamap of row
		if err := parse(dataMap); err != nil {
			return err
		}
	}

	return nil
}

func trimHeadRow(s string) string {
	return regexp.MustCompile(`\s+`).ReplaceAllString(strings.Trim(s, " \n	"), " ")
}

func getSpacerLength(s string) int {
	result := 0
	log.Debugf("getSpacerLength | input: '%s'", s)
	if match := regexp.MustCompile(`(\s+)`).FindStringSubmatch(strings.Trim(s, " \n	")); len(match) < 2 {
		log.Errorf("Error! Could not determine the length of the spacer in: '%s'", s)
		result = 0
	} else {
		result = len(match[1])
	}
	log.Debugf("getSpacerLength | output: '%d'", result)
	return result
}

func convertDateStringToTimestamp(s string) string {
	t, err := time.Parse(*dateFormat, s)
	if err != nil {
		return s
	}
	return fmt.Sprint(t.Unix())
}

// If Siebel gives us some ugly names back, this function cleans it up for Prometheus.
func cleanName(s string) string {
	s = strings.Replace(s, " ", "_", -1) // Remove spaces
	s = strings.Replace(s, "(", "", -1)  // Remove open parenthesis
	s = strings.Replace(s, ")", "", -1)  // Remove close parenthesis
	s = strings.Replace(s, "/", "", -1)  // Remove forward slashes
	s = strings.Replace(s, "*", "", -1)  // Remove asterisks
	s = strings.ToLower(s)
	return s
}

func hashFile(h hash.Hash, fn string) error {
	f, err := os.Open(fn)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(h, f); err != nil {
		return err
	}
	return nil
}

func checkIfMetricsChanged() bool {
	log.Debugln("Check if metrics changed...")
	result := false

	for i, _customMetrics := range strings.Split(*customMetricsFile, ",") {
		if len(_customMetrics) == 0 {
			continue
		}
		log.Debug("Checking modifications in following metrics definition file:", _customMetrics)
		h := sha256.New()
		if err := hashFile(h, _customMetrics); err != nil {
			log.Errorln("Unable to get file hash", err)
			result = false
			break
		}
		// If any of files has been changed reload metrics
		if !bytes.Equal(hashMap[i], h.Sum(nil)) {
			log.Infoln(_customMetrics, "has been changed. Reloading metrics...")
			hashMap[i] = h.Sum(nil)
			result = true
			break
		}
	}

	if result {
		log.Debugln("Metrics changed.")
	} else {
		log.Debugln("Metrics not changed.")
	}

	return result
}

func reloadMetrics() {
	// Truncate metricsToScrap
	metricsToScrap.Metric = []Metric{}

	// Load default metrics
	if _, err := toml.DecodeFile(*defaultMetricsFile, &metricsToScrap); err != nil {
		log.Errorln(err)
		panic(errors.New("Error while loading " + *defaultMetricsFile))
	} else {
		log.Infoln("Successfully loaded default metrics from: " + *defaultMetricsFile)
	}

	// If custom metrics, load it
	if strings.Compare(*customMetricsFile, "") != 0 {
		for _, _customMetrics := range strings.Split(*customMetricsFile, ",") {
			if _, err := toml.DecodeFile(_customMetrics, &additionalMetrics); err != nil {
				log.Errorln(err)
				panic(errors.New("Error while loading " + _customMetrics))
			} else {
				log.Infoln("Successfully loaded custom metrics from: " + _customMetrics)
			}
			metricsToScrap.Metric = append(metricsToScrap.Metric, additionalMetrics.Metric...)
		}
	} else {
		log.Infoln("No custom metrics defined.")
	}
}
