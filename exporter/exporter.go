package exporter

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"hash"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/barkadron/siebel_exporter/srvrmgr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
)

// Metrics to scrap. Use external file (default-metrics.toml and custom if provided)
var (
	metricsToScrap    Metrics
	additionalMetrics Metrics
	hashMap           = make(map[int][]byte)
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
	ValueMap         map[string]map[string]string
}

// Used to load multiple metrics from file
type Metrics struct {
	Metric []Metric
}

// Exporter collects Siebel metrics. It implements prometheus.Collector.
type Exporter struct {
	namespace            string
	subsystem            string
	dateFormat           string
	emptyMetricsOverride string
	defaultMetricsFile   string
	customMetricsFile    string
	srvrmgr              srvrmgr.SrvrMgr
	duration, error      prometheus.Gauge
	totalScrapes         prometheus.Counter
	scrapeErrors         *prometheus.CounterVec
	gatewayServerUp      prometheus.Gauge
	// appServerUp     prometheus.Gauge
}

// NewExporter returns a new Siebel exporter for the provided args.
func NewExporter(namespace, subsystem, defaultMetricsFile, customMetricsFile, dateFormat, emptyMetricsOverride string, srvrmgr srvrmgr.SrvrMgr) *Exporter {
	// log.Infoln("Creating new Exporter...")

	// Load default and custom metrics
	reloadMetrics(&defaultMetricsFile, &customMetricsFile)

	// maybe this is superfluous...
	// labels := map[string]string{
	// 	"server_name": srvrmgr.getApplicationServerName(),
	// }

	return &Exporter{
		namespace:            namespace,
		subsystem:            subsystem,
		dateFormat:           dateFormat,
		emptyMetricsOverride: emptyMetricsOverride,
		defaultMetricsFile:   defaultMetricsFile,
		customMetricsFile:    customMetricsFile,
		srvrmgr:              srvrmgr,
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

	if checkIfMetricsChanged(&e.customMetricsFile) {
		log.Infoln("Metrics changed, reload it.")
		reloadMetrics(&e.defaultMetricsFile, &e.customMetricsFile)
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
		log.Debugln("	- Metric ValueMap: ", metric.ValueMap)
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
		if err = ScrapeMetric(e.namespace, e.dateFormat, e.emptyMetricsOverride, e.srvrmgr, ch, metric); err != nil {
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
func ScrapeMetric(namespace string, dateFormat string, overrideEmptyMetricsValue string, srvrmgr srvrmgr.SrvrMgr, ch chan<- prometheus.Metric, metricDefinition Metric) error {
	return ScrapeGenericValues(namespace, dateFormat, overrideEmptyMetricsValue, srvrmgr, ch, metricDefinition.Context, metricDefinition.Labels,
		metricDefinition.MetricsDesc, metricDefinition.MetricsType, metricDefinition.MetricsBuckets, metricDefinition.FieldToAppend,
		metricDefinition.IgnoreZeroResult, metricDefinition.ValueMap, metricDefinition.Request)
}

// generic method for retrieving metrics.
func ScrapeGenericValues(namespace string, dateFormat string, overrideEmptyMetricsValue string, srvrmgr srvrmgr.SrvrMgr, ch chan<- prometheus.Metric, context string, labels []string,
	metricsDesc map[string]string, metricsType map[string]string, metricsBuckets map[string]map[string]string, fieldToAppend string, ignoreZeroResult bool, valueMap map[string]map[string]string, request string) error {
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
			value := row[metric]
			// Value mapping
			if val1, exists1 := valueMap[metric]; exists1 {
				if val2, exists2 := val1[value]; exists2 {
					log.Debugln("[ValueMap]: Value '" + value + "' converted to '" + val2 + "'.")
					value = val2
				}
			}
			// If not a float, skip current metric
			parsedValue, err := strconv.ParseFloat(value, 64)
			if err != nil {
				log.Errorln("Unable to convert current value to float (metric=" + metric + ",metricHelp=" + metricHelp + ",value=<" + value + ">)")
				continue
			}
			log.Debugln("Request result looks like: [", metric, "] : ", parsedValue)
			// If metric do not use a field content in metric's name
			if strings.Compare(fieldToAppend, "") == 0 {
				promLabels := []string{}
				for _, label := range labels {
					promLabels = append(promLabels, strings.ToLower(label))
				}
				desc := prometheus.NewDesc(
					prometheus.BuildFQName(namespace, context, cleanName(metric)),
					metricHelp,
					promLabels,
					nil,
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
					ch <- prometheus.MustNewConstHistogram(desc, count, parsedValue, buckets, labelsValues...)
				} else {
					ch <- prometheus.MustNewConstMetric(desc, GetMetricType(metric, metricsType), parsedValue, labelsValues...)
				}
				// If no labels, use metric name
			} else {
				desc := prometheus.NewDesc(
					prometheus.BuildFQName(namespace, context, cleanName(row[fieldToAppend])),
					metricHelp,
					nil,
					nil,
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
					ch <- prometheus.MustNewConstHistogram(desc, count, parsedValue, buckets)
				} else {
					ch <- prometheus.MustNewConstMetric(desc, GetMetricType(metric, metricsType), parsedValue)
				}
			}
			metricsCount++
		}
		return nil
	}
	err := GeneratePrometheusMetrics(srvrmgr, genericParser, request, dateFormat, overrideEmptyMetricsValue)
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
func GeneratePrometheusMetrics(srvrmgr srvrmgr.SrvrMgr, parse func(row map[string]string) error, request string, dateFormat string, overrideEmptyMetricsValue string) error {
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
			if len(val) == 0 && len(overrideEmptyMetricsValue) > 0 {
				val = overrideEmptyMetricsValue
			}

			// Try to convert date-string to Unix timestamp
			if len(val) == len(dateFormat) {
				val = convertDateStringToTimestamp(val, dateFormat)
			}

			// dataMap[strings.ToLower(colName)] = val
			dataMap[colName] = val

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

func convertDateStringToTimestamp(s string, dateFormat string) string {
	t, err := time.Parse(dateFormat, s)
	if err != nil {
		return s
	}
	return fmt.Sprint(t.Unix())
}

// If Siebel gives us some ugly names back, this function cleans it up for Prometheus.
func cleanName(s string) string {
	s = strings.TrimSpace(s)             // Trim spaces
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

func checkIfMetricsChanged(customMetricsFile *string) bool {
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

func reloadMetrics(defaultMetricsFile, customMetricsFile *string) {
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
