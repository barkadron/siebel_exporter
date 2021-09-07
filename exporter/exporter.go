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

// Metrics object definition
type Metric struct {
	Command          string
	Subsystem        string
	Help             map[string]string
	HelpField        map[string]string
	Type             map[string]string
	Buckets          map[string]map[string]string
	ValueMap         map[string]map[string]string
	Labels           []string
	FieldToAppend    string
	IgnoreZeroResult bool
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
	overrideEmptyMetrics bool
	defaultMetricsFile   string
	customMetricsFile    string
	srvrmgr              srvrmgr.SrvrMgr
	duration, error      prometheus.Gauge
	totalScrapes         prometheus.Counter
	// scrapeErrors         *prometheus.CounterVec
	scrapeErrors    prometheus.Counter
	gatewayServerUp prometheus.Gauge
}

var (
	defaultMetrics Metrics                // Default metrics to scrap. Use external file (default-metrics.toml)
	customMetrics  Metrics                // Custom metrics to scrap. Use custom external file (if provided)
	metricsHashMap = make(map[int][]byte) // Metrics Files HashMap
)

// NewExporter returns a new Siebel exporter for the provided args.
func NewExporter(srvrmgr srvrmgr.SrvrMgr, defaultMetricsFile, customMetricsFile, dateFormat string, overrideEmptyMetrics bool) *Exporter {
	log.Debugln("NewExporter")

	const (
		namespace = "siebel"
		subsystem = "exporter"
	)

	// Load default and custom metrics
	reloadMetrics(defaultMetricsFile, customMetricsFile)

	// maybe this is superfluous...
	// labels := map[string]string{
	// 	"server_name": srvrmgr.getApplicationServerName(),
	// }

	return &Exporter{
		namespace:            namespace,
		subsystem:            subsystem,
		dateFormat:           dateFormat,
		overrideEmptyMetrics: overrideEmptyMetrics,
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
		scrapeErrors: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace,
			Subsystem: subsystem,
			Name:      "scrape_errors_total",
			Help:      "Total number of times an error occured scraping a Siebel.",
			// ConstLabels: labels,
			// }, []string{"collector"}),
		}),
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
	log.Debugln("Exporter.Collect")
	e.scrape(ch)
	ch <- e.duration
	ch <- e.totalScrapes
	ch <- e.error
	e.scrapeErrors.Collect(ch)
	ch <- e.gatewayServerUp
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

	e.gatewayServerUp.Set(0)

	// Check srvrmgr connection status
	switch e.srvrmgr.GetStatus() {
	case srvrmgr.Disconnecting:
		log.Warnln("Unable to scrape: srvrmgr is in process of disconnection from Siebel Gateway Server.")
		return
	case srvrmgr.Connecting:
		log.Warnln("Unable to scrape: srvrmgr is in process of connection to Siebel Gateway Server.")
		return
	case srvrmgr.Disconnect:
		log.Warnln("Unable to scrape: srvrmgr not connected to Siebel Gateway Server. Trying to connect...")
		if err = e.srvrmgr.Connect(); err != nil {
			return
		}
	case srvrmgr.Connected:
		if !e.srvrmgr.PingGatewayServer() {
			log.Warnln("Unable to scrape: srvrmgr was lost connection to the Siebel Gateway Server. Will try to reconnect on next scrape.")
			return
		}
	default:
		log.Errorln("Unable to scrape: unknown status of srvrmgr connection.")
		return
	}

	e.gatewayServerUp.Set(1)

	if checkIfMetricsChanged(e.customMetricsFile) {
		log.Infoln("Custom metrics changed, reload it.")
		reloadMetrics(e.defaultMetricsFile, e.customMetricsFile)
	}

	for _, metric := range defaultMetrics.Metric {
		log.Debugln("About to scrape metric: ")
		log.Debugln("	- Metric Command: ", metric.Command)
		log.Debugln("	- Metric Subsystem: ", metric.Subsystem)
		log.Debugln("	- Metric Help: ", metric.Help)
		log.Debugln("	- Metric HelpField: ", metric.HelpField)
		log.Debugln("	- Metric Type: ", metric.Type)
		log.Debugln("	- Metric Buckets: ", metric.Buckets, "(Ignored unless histogram type)")
		log.Debugln("	- Metric ValueMap: ", metric.ValueMap)
		log.Debugln("	- Metric Labels: ", metric.Labels)
		log.Debugln("	- Metric FieldToAppend: ", metric.FieldToAppend)
		log.Debugln("	- Metric IgnoreZeroResult: ", metric.IgnoreZeroResult)

		if len(metric.Command) == 0 {
			log.Errorln("Error scraping for '" + fmt.Sprintf("%+v", metric.Help) + "'. Did you forget to define 'command' in your toml file?")
			return
		}

		if len(metric.Help) == 0 {
			log.Errorln("Error scraping for command '" + metric.Command + "'. Did you forget to define 'help' in your toml file?")
			return
		}

		for columnName, metricType := range metric.Type {
			if strings.ToLower(metricType) == "histogram" {
				if len(metric.Buckets) == 0 {
					log.Errorln("Error scraping for command '" + metric.Command + "'. Did you forget to define 'buckets' in your toml file?")
					return
				}
				_, exists := metric.Buckets[columnName]
				if !exists {
					log.Errorln("Error scraping for command '" + metric.Command + "'. Unable to find buckets configuration key for metric '" + columnName + "'.")
					return
				}
			}
		}

		scrapeStart := time.Now()
		if err = scrapeMetric(e.namespace, e.dateFormat, e.overrideEmptyMetrics, e.srvrmgr, ch, metric); err != nil {
			log.Errorln("Error scraping for '" + metric.Subsystem + "', '" + fmt.Sprintf("%+v", metric.Help) + "' :\n" + err.Error())
			// e.scrapeErrors.WithLabelValues(metric.Subsystem).Inc()
			e.scrapeErrors.Inc()
		} else {
			scrapeEnd := time.Since(scrapeStart)
			log.Debugln("Successfully scraped metric. '" + metric.Subsystem + "', " + fmt.Sprintf("%+v", metric.Help) + "'. Time: '" + scrapeEnd.String() + "'.")
		}
	}
}

// interface method to call ScrapeGenericValues using Metric struct values
func scrapeMetric(namespace string, dateFormat string, overrideEmptyMetrics bool, srvrmgr srvrmgr.SrvrMgr, ch chan<- prometheus.Metric, metricDefinition Metric) error {
	return scrapeGenericValues(namespace, dateFormat, overrideEmptyMetrics, srvrmgr, ch,
		metricDefinition.Subsystem, metricDefinition.Labels, metricDefinition.Help, metricDefinition.HelpField, metricDefinition.Type,
		metricDefinition.Buckets, metricDefinition.FieldToAppend, metricDefinition.IgnoreZeroResult, metricDefinition.ValueMap, metricDefinition.Command)
}

// generic method for retrieving metrics.
func scrapeGenericValues(namespace string, dateFormat string, overrideEmptyMetrics bool, srvrmgr srvrmgr.SrvrMgr, ch chan<- prometheus.Metric,
	metricSubsystem string, labels []string, metricsHelp map[string]string, metricsHelpField map[string]string, metricsTypes map[string]string,
	metricsBuckets map[string]map[string]string, fieldToAppend string, ignoreZeroResult bool, valueMap map[string]map[string]string, command string) error {
	metricsCount := 0
	dataRowToPrometheusMetricConverter := func(row map[string]string) error {
		log.Debugln("dataRowToPrometheusMetricConverter")
		log.Debugln("	- row map : '" + fmt.Sprintf("%+v", row) + "'")

		// Construct labels name and value
		labelsNamesCleaned := []string{}
		labelsValues := []string{}
		// if strings.Compare(fieldToAppend, "") == 0 {
		for _, label := range labels {
			labelsNamesCleaned = append(labelsNamesCleaned, cleanName(label))
			labelsValues = append(labelsValues, row[label])
		}
		// }
		// Construct Prometheus values to sent back
		for metricName, metricHelp := range metricsHelp {
			metricType := getMetricType(metricName, metricsTypes)
			metricNameCleaned := cleanName(metricName)
			if strings.Compare(fieldToAppend, "") != 0 {
				metricNameCleaned = cleanName(row[fieldToAppend])
			}
			// Dinamic help
			if dinHelpName, exists1 := metricsHelpField[metricName]; exists1 {
				if dinHelpValue, exists2 := row[dinHelpName]; exists2 {
					log.Debugln("	- [DinamicHelp]: Help value '" + metricHelp + "' append with dinamic value '" + dinHelpValue + "'.")
					metricHelp = metricHelp + " " + dinHelpValue
				}
			}
			metricValue := row[metricName]
			// Value mapping
			if val1, exists1 := valueMap[metricName]; exists1 {
				if val2, exists2 := val1[metricValue]; exists2 {
					log.Debugln("	- [ValueMap]: Value '" + metricValue + "' converted to '" + val2 + "'.")
					metricValue = val2
				}
			}
			// If not a float, skip current metric
			metricValueParsed, err := strconv.ParseFloat(metricValue, 64)
			if err != nil {
				log.Errorln("Unable to convert current value to float (metricName='" + metricName + "', value='" + metricValue + "', metricHelp='" + metricHelp + "').")
				continue
			}

			promMetricDesc := prometheus.NewDesc(prometheus.BuildFQName(namespace, metricSubsystem, metricNameCleaned), metricHelp, labelsNamesCleaned, nil)

			if metricType == prometheus.GaugeValue || metricType == prometheus.CounterValue {
				log.Debugln("	- Converting result looks like: [" + metricNameCleaned + "] : '" + fmt.Sprintf("%g", metricValueParsed) + "'.")
				ch <- prometheus.MustNewConstMetric(promMetricDesc, metricType, metricValueParsed, labelsValues...)
			} else {
				count, err := strconv.ParseUint(strings.TrimSpace(row["count"]), 10, 64)
				if err != nil {
					log.Errorln("Unable to convert count value to int (metricName='" + metricName + "', value='" + row["count"] + "', metricHelp='" + metricHelp + "').")
					continue
				}
				buckets := make(map[float64]uint64)
				for field, le := range metricsBuckets[metricName] {
					lelimit, err := strconv.ParseFloat(strings.TrimSpace(le), 64)
					if err != nil {
						log.Errorln("Unable to convert bucket limit value to float (metricName='" + metricName + "', bucketlimit='" + le + "', metricHelp='" + metricHelp + "')")
						continue
					}
					counter, err := strconv.ParseUint(strings.TrimSpace(row[field]), 10, 64)
					if err != nil {
						log.Errorln("Unable to convert <" + field + "> value to int (metricName='" + metricName + "', value='" + row[field] + "', metricHelp='" + metricHelp + "')")
						continue
					}
					buckets[lelimit] = counter
				}
				log.Debugln("	- Converting result looks like: [" + metricNameCleaned + "] : '" + fmt.Sprintf("%g", metricValueParsed) + "', count: '" + fmt.Sprint(count) + ", buckets: '" + fmt.Sprintf("%+v", buckets) + "'.")
				ch <- prometheus.MustNewConstHistogram(promMetricDesc, count, metricValueParsed, buckets, labelsValues...)
			}
			metricsCount++
		}
		return nil
	}

	err := generatePrometheusMetrics(srvrmgr, dataRowToPrometheusMetricConverter, command, dateFormat, overrideEmptyMetrics)
	log.Debugln("ScrapeGenericValues | metricsCount: " + fmt.Sprint(metricsCount))
	if err != nil {
		return err
	}
	if !ignoreZeroResult && metricsCount == 0 {
		return errors.New("ERROR! No metrics found while parsing. Metrics Count: '" + fmt.Sprint(metricsCount) + "'.")
	}

	return err
}

// Parse srvrmgr result and call parsing function to each row
func generatePrometheusMetrics(srvrmgr srvrmgr.SrvrMgr, dataRowToPrometheusMetricConverter func(row map[string]string) error, command string, dateFormat string, overrideEmptyMetrics bool) error {
	log.Debugln("generatePrometheusMetrics")

	commandResult, err := srvrmgr.ExecuteCommand(command)
	if err != nil {
		return err
	}

	// Check and parse srvrmgr output...
	lines := strings.Split(commandResult, "\n")
	if len(lines) < 3 {
		return errors.New("ERROR! Command output is not valid: '" + commandResult + "'.")
	}

	columnsRow := lines[0]
	separatorsRow := lines[1]
	rawDataRows := lines[2:]

	// Get column names
	columnsNames := strings.Split(trimHeadRow(columnsRow), " ")

	// Get column max lengths (calc from separator length)
	spacerLength := getSpacerLength(separatorsRow)
	separators := strings.Split(trimHeadRow(separatorsRow), " ")
	lengths := make([]int, len(separators))
	for i, s := range separators {
		lengths[i] = len(s) + spacerLength
	}

	// Parse data-rows
	log.Debugln("Parsing rows with data...")
	for _, rawRow := range rawDataRows {
		log.Debugln("	- raw row: '" + rawRow + "'.")

		parsedRow := make(map[string]string)
		rowLen := len(rawRow)
		for colIndex, colName := range columnsNames {
			colMaxLen := lengths[colIndex]
			if colMaxLen > rowLen {
				colMaxLen = rowLen
			}
			colValue := strings.TrimSpace(rawRow[:colMaxLen])

			// If value is empty then set it to default "0"
			if overrideEmptyMetrics && len(colValue) == 0 {
				colValue = "0"
			}

			// Try to convert date-string to Unix timestamp
			if len(colValue) == len(dateFormat) {
				colValue = convertDateStringToTimestamp(colValue, dateFormat)
			}

			// dataMap[strings.ToLower(colName)] = colValue
			parsedRow[colName] = colValue

			// Cut off used value from row
			rawRow = rawRow[colMaxLen:]
			rowLen = len(rawRow)
		}

		// Convert parsed row to Prometheus Metric
		if err := dataRowToPrometheusMetricConverter(parsedRow); err != nil {
			return err
		}
	}

	return nil
}

func getMetricType(metricName string, metricsTypes map[string]string) prometheus.ValueType {
	var strToPromType = map[string]prometheus.ValueType{
		"gauge":     prometheus.GaugeValue,
		"counter":   prometheus.CounterValue,
		"histogram": prometheus.UntypedValue,
	}
	strType, exists := metricsTypes[metricName]
	if !exists {
		return prometheus.GaugeValue
	}
	strType = strings.ToLower(strType)
	valueType, exists := strToPromType[strType]
	if !exists {
		panic(errors.New("Error while getting prometheus type for '" + strType + "'."))
	}
	return valueType
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
	if s == "0000-00-00 00:00:00" {
		return "0"
	}
	t, err := time.Parse(dateFormat, s)
	if err != nil {
		return s
	}
	return fmt.Sprint(t.Unix())
}

// If Siebel gives us some ugly names back, this function cleans it up for Prometheus.
// https://prometheus.io/docs/concepts/data_model/#metric-names-and-labels
func cleanName(s string) string {
	s = strings.TrimSpace(s)                                        // Trim spaces
	s = strings.Replace(s, " ", "_", -1)                            // Remove spaces
	s = regexp.MustCompile(`[^a-zA-Z0-9_]`).ReplaceAllString(s, "") // Remove other bad chars
	s = strings.ToLower(s)                                          // Switch case to lower
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

func checkIfMetricsChanged(customMetricsFile string) bool {
	log.Debugln("Check if metrics changed...")
	result := false

	for i, _customMetrics := range strings.Split(customMetricsFile, ",") {
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
		if !bytes.Equal(metricsHashMap[i], h.Sum(nil)) {
			log.Infoln(_customMetrics, "has been changed. Reloading metrics...")
			metricsHashMap[i] = h.Sum(nil)
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

func reloadMetrics(defaultMetricsFile, customMetricsFile string) {
	// Truncate defaultMetrics
	defaultMetrics.Metric = []Metric{}

	// Load default metrics
	if _, err := toml.DecodeFile(defaultMetricsFile, &defaultMetrics); err != nil {
		log.Errorln(err)
		panic(errors.New("Error while loading " + defaultMetricsFile))
	} else {
		log.Infoln("Successfully loaded default metrics from: " + defaultMetricsFile)
	}

	// If custom metrics, load it
	if strings.Compare(customMetricsFile, "") != 0 {
		for _, _customMetrics := range strings.Split(customMetricsFile, ",") {
			if _, err := toml.DecodeFile(_customMetrics, &customMetrics); err != nil {
				log.Errorln(err)
				panic(errors.New("Error while loading " + _customMetrics))
			} else {
				log.Infoln("Successfully loaded custom metrics from: " + _customMetrics)
			}
			defaultMetrics.Metric = append(defaultMetrics.Metric, customMetrics.Metric...)
		}
	} else {
		log.Infoln("No custom metrics defined.")
	}
}
