package main

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"hash"
	"io"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/common/log"
	"github.com/prometheus/common/version"
	"golang.org/x/text/encoding/charmap"
	"gopkg.in/alecthomas/kingpin.v2"
)

const (
	namespace = "siebel" 
	subsystem  = "exporter"
	exporterName = namespace + "_" + subsystem
)

var (
	listenAddress = kingpin.Flag("web.listen-address", "Address to listen on for web interface and telemetry. (env: LISTEN_ADDRESS)").Default(getEnv("LISTEN_ADDRESS", ":28001")).String()
	telemetryPath = kingpin.Flag("web.telemetry-path", "Path under which to expose metrics. (env: TELEMETRY_PATH)").Default(getEnv("TELEMETRY_PATH", "/metrics")).String()
	defaultFileMetrics = kingpin.Flag("default.metrics", "File with default metrics in a TOML file. (env: DEFAULT_METRICS)").Default(getEnv("DEFAULT_METRICS", "default-metrics.toml")).String()
	customMetrics = kingpin.Flag("custom.metrics", "File that may contain various custom metrics in a TOML file. (env: CUSTOM_METRICS)").Default(getEnv("CUSTOM_METRICS", "")).String()
	queryTimeout = kingpin.Flag("query.timeout", "Query timeout (in seconds). (env: QUERY_TIMEOUT)").Default(getEnv("QUERY_TIMEOUT", "5")).String()
	securedMetrics = kingpin.Flag("web.secured-metrics", "Expose metrics using https.").Default("false").Bool()
	serverCert = kingpin.Flag("web.ssl-server-cert", "Path to the PEM encoded certificate").ExistingFile()
	serverKey = kingpin.Flag("web.ssl-server-key", "Path to the PEM encoded key").ExistingFile()

	gracefulStop = make(chan os.Signal)
)

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
	// dsn             string
	duration, error prometheus.Gauge
	totalScrapes    prometheus.Counter
	scrapeErrors    *prometheus.CounterVec
	up              prometheus.Gauge
}

// getEnv returns the value of an environment variable, or returns the provided fallback value
func getEnv(key, fallback string) string {
	if value, ok := os.LookupEnv(key); ok {
		return value
	}
	return fallback
}

// NewExporter returns a new Siebel exporter for the provided args.
func NewExporter(/*dsn string*/) *Exporter {
	// db := connect(dsn)
	return &Exporter{
		// dsn: dsn,
		duration: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "last_scrape_duration_seconds",
			Help:      "Duration of the last scrape of metrics from Siebel.",
		}),
		totalScrapes: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "scrapes_total",
			Help:      "Total number of times Siebel was scraped for metrics.",
		}),
		scrapeErrors: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "scrape_errors_total",
			Help:      "Total number of times an error occured scraping a Siebel.",
		}, []string{"collector"}),
		error: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "last_scrape_error",
			Help:      "Whether the last scrape of metrics from Siebel resulted in an error (1 for error, 0 for success).",
		}),
		up: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "up",
			Help:      "Whether the Siebel is up (1 for up, 0 for down).",
		}),
		// db: db,
	}
}

// Describe describes all the metrics exported by the Siebel exporter.
func (e *Exporter) Describe(ch chan<- *prometheus.Desc) {
	// We cannot know in advance what metrics the exporter will generate
	// So we use the poor man's describe method: Run a collect
	// and send the descriptors of all the collected metrics. The problem
	// here is that we need to connect to the Siebel. If it is currently
	// unavailable, the descriptors will be incomplete. Since this is a
	// stand-alone exporter and not used as a library within other code
	// implementing additional metrics, the worst that can happen is that we
	// don't detect inconsistent metrics created by this exporter
	// itself. Also, a change in the monitored Siebel instance may change the
	// exported metrics during the runtime of the exporter.

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
	ch <- e.up
}

func (e *Exporter) scrape(ch chan<- prometheus.Metric) {
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

	// @TODO: need to detect Siebel is up or down

/*
	if err = e.db.Ping(); err != nil {
		if strings.Contains(err.Error(), "sql: database is closed") {
			log.Infoln("Reconnecting to DB")
			e.db = connect(e.dsn)
		}
	}
	if err = e.db.Ping(); err != nil {
		log.Errorln("Error pinging oracle:", err)
		//e.db.Close()
		e.up.Set(0)
		return
	} else {
		log.Debugln("Successfully pinged Oracle database: ")
		e.up.Set(1)
	}
*/

	if checkIfMetricsChanged() {
		reloadMetrics()
	}

	wg := sync.WaitGroup{}

	for _, metric := range metricsToScrap.Metric {
		wg.Add(1)
		metric := metric //https://golang.org/doc/faq#closures_and_goroutines

		go func() {
			defer wg.Done()

			log.Debugln("About to scrape metric: ")
			log.Debugln("- Metric MetricsDesc: ", metric.MetricsDesc)
			log.Debugln("- Metric Context: ", metric.Context)
			log.Debugln("- Metric MetricsType: ", metric.MetricsType)
			log.Debugln("- Metric MetricsBuckets: ", metric.MetricsBuckets, "(Ignored unless Histogram type)")
			log.Debugln("- Metric Labels: ", metric.Labels)
			log.Debugln("- Metric FieldToAppend: ", metric.FieldToAppend)
			log.Debugln("- Metric IgnoreZeroResult: ", metric.IgnoreZeroResult)
			log.Debugln("- Metric Request: ", metric.Request)

			if len(metric.Request) == 0 {
				log.Errorln("Error scraping for ", metric.MetricsDesc, ". Did you forget to define request in your toml file?")
				return
			}

			if len(metric.MetricsDesc) == 0 {
				log.Errorln("Error scraping for query", metric.Request, ". Did you forget to define metricsdesc in your toml file?")
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
			if err = ScrapeMetric(/*e.db, */ch, metric); err != nil {
				log.Errorln("Error scraping for", metric.Context, "_", metric.MetricsDesc, ":", err)
				e.scrapeErrors.WithLabelValues(metric.Context).Inc()
			} else {
				log.Debugln("Successfully scraped metric: ", metric.Context, metric.MetricsDesc, time.Since(scrapeStart))
			}
		}()
	}
	wg.Wait()
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
func ScrapeMetric(/*db *sql.DB, */ch chan<- prometheus.Metric, metricDefinition Metric) error {
	log.Debugln("Calling function ScrapeGenericValues()")
	return ScrapeGenericValues(/*db, */ch, metricDefinition.Context, metricDefinition.Labels,
		metricDefinition.MetricsDesc, metricDefinition.MetricsType, metricDefinition.MetricsBuckets,
		metricDefinition.FieldToAppend, metricDefinition.IgnoreZeroResult,
		metricDefinition.Request)
}

// generic method for retrieving metrics.
func ScrapeGenericValues(/*db *sql.DB, */ch chan<- prometheus.Metric, context string, labels []string,
	metricsDesc map[string]string, metricsType map[string]string, metricsBuckets map[string]map[string]string, fieldToAppend string, ignoreZeroResult bool, request string) error {
	metricsCount := 0

	// @FIXME: parser for oracle must be replaced with parser for siebel
	genericParser := func(row map[string]string) error {
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
				log.Errorln("Unable to convert current value to float (metric=" + metric +
					",metricHelp=" + metricHelp + ",value=<" + row[metric] + ">)")
				continue
			}
			log.Debugln("Query result looks like: ", value)
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
						log.Errorln("Unable to convert count value to int (metric=" + metric +
							",metricHelp=" + metricHelp + ",value=<" + row["count"] + ">)")
						continue
					}
					buckets := make(map[float64]uint64)
					for field, le := range metricsBuckets[metric] {
						lelimit, err := strconv.ParseFloat(strings.TrimSpace(le), 64)
						if err != nil {
							log.Errorln("Unable to convert bucket limit value to float (metric=" + metric +
								",metricHelp=" + metricHelp + ",bucketlimit=<" + le + ">)")
							continue
						}
						counter, err := strconv.ParseUint(strings.TrimSpace(row[field]), 10, 64)
						if err != nil {
							log.Errorln("Unable to convert ", field, " value to int (metric="+metric+
								",metricHelp="+metricHelp+",value=<"+row[field]+">)")
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
						log.Errorln("Unable to convert count value to int (metric=" + metric +
							",metricHelp=" + metricHelp + ",value=<" + row["count"] + ">)")
						continue
					}
					buckets := make(map[float64]uint64)
					for field, le := range metricsBuckets[metric] {
						lelimit, err := strconv.ParseFloat(strings.TrimSpace(le), 64)
						if err != nil {
							log.Errorln("Unable to convert bucket limit value to float (metric=" + metric +
								",metricHelp=" + metricHelp + ",bucketlimit=<" + le + ">)")
							continue
						}
						counter, err := strconv.ParseUint(strings.TrimSpace(row[field]), 10, 64)
						if err != nil {
							log.Errorln("Unable to convert ", field, " value to int (metric="+metric+
								",metricHelp="+metricHelp+",value=<"+row[field]+">)")
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
	err := GeneratePrometheusMetrics(/*db, */genericParser, request)
	log.Debugln("ScrapeGenericValues() - metricsCount: ", metricsCount)
	if err != nil {
		return err
	}
	if !ignoreZeroResult && metricsCount == 0 {
		return errors.New("no metrics found while parsing")
	}
	return err
}

// inspired by https://kylewbanks.com/blog/query-result-to-map-in-golang
// Parse SQL result and call parsing function to each row
func GeneratePrometheusMetrics(/*db *sql.DB, */parse func(row map[string]string) error, query string) error {

	log.Debug("query: " + query)

	// Add a timeout
	timeout, err := strconv.Atoi(*queryTimeout)
	if err != nil {
		log.Fatal("error while converting timeout option value: ", err)
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()

	// @FIXME: Need universal invoke for Linux + Windows
    res, err := exec.CommandContext(ctx, "cmd.exe", "/C", query).Output()

	if ctx.Err() == context.DeadlineExceeded {
		return errors.New("srvrmgr query timed out")
	}

	if err != nil {
		return err
	}

	// @FIXME: Codepage for Russian words in Windows cmd
	// https://www.reddit.com/r/golang/comments/9zsipj/help_osexec_output_on_nonenglish_windows_cmd/
	d := charmap.CodePage866.NewDecoder()
    out, err := d.Bytes(res)
    if err != nil {
        panic(err)
    }

    log.Infoln("RESULT: " + string(out))


	// @ TODO: now need to parse srvrmgr output


	/*	example :

	// Add a timeout
	timeout, err := strconv.Atoi(*queryTimeout)
	if err != nil {
		log.Fatal("error while converting timeout option value: ", err)
		panic(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(timeout)*time.Second)
	defer cancel()
	rows, err := db.QueryContext(ctx, query)

	if ctx.Err() == context.DeadlineExceeded {
		return errors.New("Oracle query timed out")
	}

	if err != nil {
		return err
	}
	cols, err := rows.Columns()
	defer rows.Close()

	for rows.Next() {
		// Create a slice of interface{}'s to represent each column,
		// and a second slice to contain pointers to each item in the columns slice.
		columns := make([]interface{}, len(cols))
		columnPointers := make([]interface{}, len(cols))
		for i, _ := range columns {
			columnPointers[i] = &columns[i]
		}

		// Scan the result into the column pointers...
		if err := rows.Scan(columnPointers...); err != nil {
			return err
		}

		// Create our map, and retrieve the value for each column from the pointers slice,
		// storing it in the map with the name of the column as the key.
		m := make(map[string]string)
		for i, colName := range cols {
			val := columnPointers[i].(*interface{})
			m[strings.ToLower(colName)] = fmt.Sprintf("%v", *val)
		}
		// Call function to parse row
		if err := parse(m); err != nil {
			return err
		}
	}
*/
	return nil

}

// Siebel gives us some ugly names back. This function cleans things up for Prometheus.
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
	for i, _customMetrics := range strings.Split(*customMetrics, ",") {
		if len(_customMetrics) == 0 {
			continue
		}
		log.Debug("Checking modifications in following metrics definition file:", _customMetrics)
		h := sha256.New()
		if err := hashFile(h, _customMetrics); err != nil {
			log.Errorln("Unable to get file hash", err)
			return false
		}
		// If any of files has been changed reload metrics
		if !bytes.Equal(hashMap[i], h.Sum(nil)) {
			log.Infoln(_customMetrics, "has been changed. Reloading metrics...")
			hashMap[i] = h.Sum(nil)
			return true
		}
	}
	return false
}

func reloadMetrics() {
	// Truncate metricsToScrap
	metricsToScrap.Metric = []Metric{}

	// Load default metrics
	if _, err := toml.DecodeFile(*defaultFileMetrics, &metricsToScrap); err != nil {
		log.Errorln(err)
		panic(errors.New("Error while loading " + *defaultFileMetrics))
	} else {
		log.Infoln("Successfully loaded default metrics from: " + *defaultFileMetrics)
	}

	// If custom metrics, load it
	if strings.Compare(*customMetrics, "") != 0 {
		for _, _customMetrics := range strings.Split(*customMetrics, ",") {
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

func main() {
	// Parse flags
	log.AddFlags(kingpin.CommandLine)
	kingpin.Version(version.Print(exporterName))
	kingpin.HelpFlag.Short('h')
	kingpin.Parse()

	// Listen to termination signals from the OS
	signal.Notify(gracefulStop, syscall.SIGTERM,  syscall.SIGINT, syscall.SIGHUP, syscall.SIGQUIT)

	// @FIXME: version Info and BuildContext is empty
	// (version=, branch=, revision=)
	// (go=go1.16.4, user=, date=)
	// https://www.digitalocean.com/community/tutorials/using-ldflags-to-set-version-information-for-go-applications-ru
	log.Infoln("Starting " + exporterName, version.Info())
	log.Infoln("Build context: ", version.BuildContext())

	// @FIXME: need to gracefully stop http(s) server
	// https://medium.com/honestbee-tw-engineer/gracefully-shutdown-in-go-http-server-5f5e6b83da5a
	// https://medium.com/over-engineering/graceful-shutdown-with-go-http-servers-and-kubernetes-rolling-updates-6697e7db17cf

	// listener for the termination signals from the OS
	go func() {
		log.Infof("Listening and wait for graceful stop.")
		sig := <-gracefulStop
		log.Infof("Caught sig: %+v. Gracefully stopping...", sig)
		time.Sleep(1 * time.Second)
		log.Infof("Terminate %s on port %s", exporterName, *listenAddress)
		os.Exit(0)
	}()

	// Load default and custom metrics
	hashMap = make(map[int][]byte)
	reloadMetrics()

	exporter := NewExporter(/*dsn*/)
	prometheus.MustRegister(exporter)
	prometheus.MustRegister(version.NewCollector(exporterName))	

	// See more info on https://github.com/prometheus/client_golang/blob/master/prometheus/promhttp/http.go#L269
	opts := promhttp.HandlerOpts{
		ErrorLog: log.NewErrorLogger(),
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
		if err := http.ListenAndServeTLS(*listenAddress, *serverCert, *serverKey, nil); err != nil {
			log.Fatal("Failed to start the secure server:", err)
			panic(err)
		} else {
			log.Infof("You can check the metrics at https://localhost%s%s", *listenAddress, *telemetryPath)
		}
	} else {
		log.Infoln("Listening on", *listenAddress)
		log.Fatal(http.ListenAndServe(*listenAddress, nil))
		log.Infof("You can check the metrics at http://localhost%s%s", *listenAddress, *telemetryPath)
	}
}
