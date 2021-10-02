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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/BurntSushi/toml"
	"github.com/barkadron/siebel_exporter/srvrmgr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/common/log"
)

// Metric object description
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
	Extended         bool
}

// Metrics used to load multiple metrics from file
type Metrics struct {
	Metric []Metric
}

// Exporter collects Siebel metrics. It implements prometheus.Collector.
type Exporter struct {
	namespace                   string
	subsystem                   string
	dateFormat                  string
	disableEmptyMetricsOverride bool
	disableExtendedMetrics      bool
	defaultMetricsFile          string
	customMetricsFile           string
	srvrmgr                     srvrmgr.SrvrMgr
	duration, error             prometheus.Gauge
	totalScrapes                prometheus.Counter
	// scrapeErrors         *prometheus.CounterVec
	scrapeErrors        prometheus.Counter
	gatewayServerUp     prometheus.Gauge
	applicationServerUp prometheus.Gauge
}

var (
	defaultMetrics Metrics                // Default metrics to scrap. Use external file (default-metrics.toml)
	customMetrics  Metrics                // Custom metrics to scrap. Use custom external file (if provided)
	metricsHashMap = make(map[int][]byte) // Metrics Files HashMap
)

// NewExporter returns a new Siebel exporter for the provided args.
func NewExporter(srvrmgr srvrmgr.SrvrMgr, defaultMetricsFile, customMetricsFile, dateFormat string, disableEmptyMetricsOverride, disableExtendedMetrics bool) *Exporter {
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
		namespace:                   namespace,
		subsystem:                   subsystem,
		dateFormat:                  dateFormat,
		disableEmptyMetricsOverride: disableEmptyMetricsOverride,
		disableExtendedMetrics:      disableExtendedMetrics,
		defaultMetricsFile:          defaultMetricsFile,
		customMetricsFile:           customMetricsFile,
		srvrmgr:                     srvrmgr,
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
			Help:      "Total number of times an error occurred scraping a Siebel.",
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
		applicationServerUp: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "application_server_up",
			Help:      "Whether the Siebel Application Server is up (1 for up, 0 for down).",
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
	ch <- e.applicationServerUp
}

func (e *Exporter) scrape(ch chan<- prometheus.Metric) {
	log.Debugln("Exporter.scrape")

	e.totalScrapes.Inc()
	e.gatewayServerUp.Set(0)
	e.applicationServerUp.Set(0)

	var err error
	defer func(begun time.Time) {
		e.duration.Set(time.Since(begun).Seconds())
		if err == nil {
			e.error.Set(0)
		} else {
			e.error.Set(1)
		}
	}(time.Now())

	if !checkConnection(&e.srvrmgr) {
		return
	}

	if err = pingGatewayServer(&e.srvrmgr); err != nil {
		return
	}
	e.gatewayServerUp.Set(1)

	if err = pingApplicationServer(&e.srvrmgr); err != nil {
		return
	}
	e.applicationServerUp.Set(1)

	reloadMetricsIfItChanged(e.defaultMetricsFile, e.customMetricsFile)

	for _, metric := range defaultMetrics.Metric {
		logMetricDesc(metric)

		if !validateMetricDesc(metric) {
			return
		}

		if metric.Extended && e.disableExtendedMetrics {
			log.Debugln("Skip extended metric.")
			continue
		}

		scrapeStart := time.Now()
		if err = scrapeGenericValues(e.namespace, e.dateFormat, e.disableEmptyMetricsOverride, &e.srvrmgr, &ch, metric); err != nil {
			log.Errorln("Error scraping for '" + metric.Subsystem + "', '" + fmt.Sprintf("%+v", metric.Help) + "' :\n" + err.Error())
			// e.scrapeErrors.WithLabelValues(metric.Subsystem).Inc()
			e.scrapeErrors.Inc()
		} else {
			scrapeEnd := time.Since(scrapeStart)
			log.Debugln("Successfully scraped metric. '" + metric.Subsystem + "', " + fmt.Sprintf("%+v", metric.Help) + "'. Time: '" + scrapeEnd.String() + "'.")
		}
	}
}

// Check srvrmgr connection status
func checkConnection(smgr *srvrmgr.SrvrMgr) bool {
	switch (*smgr).GetStatus() {
	case srvrmgr.Connected:
		log.Debugln("srvrmgr connected to Siebel Gateway Server.")
		return true
	case srvrmgr.Disconnected:
		log.Warnln("Unable to scrape: srvrmgr not connected to Siebel Gateway Server. Trying to connect...")
		if err := (*smgr).Connect(); err != nil {
			return false
		}
		return true
	case srvrmgr.Disconnecting:
		log.Warnln("Unable to scrape: srvrmgr is in process of disconnection from Siebel Gateway Server.")
		return false
	case srvrmgr.Connecting:
		log.Warnln("Unable to scrape: srvrmgr is in process of connection to Siebel Gateway Server.")
		return false
	default:
		log.Errorln("Unable to scrape: unknown status of srvrmgr connection.")
		return false
	}
}

func pingGatewayServer(smgr *srvrmgr.SrvrMgr) error {
	log.Debugln("Ping Siebel Gateway Server...")
	if _, err := (*smgr).ExecuteCommand("list ent param MaxThreads show PA_VALUE"); err != nil {
		log.Errorln("Error pinging Siebel Gateway Server: \n", err, "\n")
		log.Warnln("Unable to scrape: srvrmgr was lost connection to the Siebel Gateway Server. Will try to reconnect on next scrape.")
		(*smgr).Disconnect()
		return err
	}
	log.Debugln("Successfully pinged Siebel Gateway Server.")
	return nil
}

func pingApplicationServer(smgr *srvrmgr.SrvrMgr) error {
	log.Debugln("Ping Siebel Application Server...")
	if _, err := (*smgr).ExecuteCommand("list comp show CC_ALIAS"); err != nil {
		log.Errorln("Error pinging Siebel Application Server: \n", err, "\n")
		log.Warnln("Unable to scrape: srvrmgr was lost connection to the Siebel Application Server. Will try to reconnect on next scrape.")
		(*smgr).Disconnect()
		return err
	}
	log.Debugln("Successfully pinged Siebel Gateway Server.")
	return nil
}

func logMetricDesc(metric Metric) {
	// @FIXME:
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
	log.Debugln("	- Metric Extended: ", metric.Extended)
}

func validateMetricDesc(metric Metric) bool {
	if len(metric.Command) == 0 {
		log.Errorln("Error scraping for '" + fmt.Sprintf("%+v", metric.Help) + "'. Did you forget to define 'command' in your toml file?")
		return false
	}

	if len(metric.Help) == 0 {
		log.Errorln("Error scraping for command '" + metric.Command + "'. Did you forget to define 'help' in your toml file?")
		return false
	}

	for columnName, metricType := range metric.Type {
		if strings.ToLower(metricType) == "histogram" {
			if len(metric.Buckets) == 0 {
				log.Errorln("Error scraping for command '" + metric.Command + "'. Did you forget to define 'buckets' in your toml file?")
				return false
			}
			_, exists := metric.Buckets[columnName]
			if !exists {
				log.Errorln("Error scraping for command '" + metric.Command + "'. Unable to find buckets configuration key for metric '" + columnName + "'.")
				return false
			}
		}
	}

	return true
}

// generic method for retrieving metrics.
func scrapeGenericValues(namespace string, dateFormat string, disableEmptyMetricsOverride bool, smgr *srvrmgr.SrvrMgr, ch *chan<- prometheus.Metric, metric Metric) error {
	log.Debugln("scrapeGenericValues")

	siebelData, err := getSiebelData(smgr, metric.Command, dateFormat, disableEmptyMetricsOverride)
	if err != nil {
		return err
	}

	metricsCount, err := generatePrometheusMetrics(siebelData, namespace, ch, metric)
	log.Debugln("metricsCount: '" + fmt.Sprint(metricsCount) + "'.")
	if err != nil {
		return err
	}

	if metricsCount == 0 && !metric.IgnoreZeroResult {
		return errors.New("ERROR! No metrics found while parsing. Metrics Count: '" + fmt.Sprint(metricsCount) + "'.")
	}

	return err
}

func getSiebelData(smgr *srvrmgr.SrvrMgr, command string, dateFormat string, disableEmptyMetricsOverride bool) ([]map[string]string, error) {
	siebelData := []map[string]string{}

	commandResult, err := (*smgr).ExecuteCommand(command)
	if err != nil {
		return nil, err
	}

	// Check and parse srvrmgr output...
	lines := strings.Split(commandResult, "\n")
	if len(lines) < 3 {
		return nil, errors.New("ERROR! Command output is not valid: '" + commandResult + "'.")
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
			if len(colValue) == 0 && !disableEmptyMetricsOverride {
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

		siebelData = append(siebelData, parsedRow)
	}

	return siebelData, nil
}

// Parse srvrmgr result and call parsing function to each row
func generatePrometheusMetrics(data []map[string]string, namespace string, ch *chan<- prometheus.Metric, metric Metric) (int, error) {
	log.Debugln("generatePrometheusMetrics")

	metricsCount := 0
	dataRowToPrometheusMetricConverter := func(row map[string]string) error {
		log.Debugln("dataRowToPrometheusMetricConverter")
		log.Debugln("	- row map : '" + fmt.Sprintf("%+v", row) + "'")

		// Construct labels name and value
		labelsNamesCleaned := []string{}
		labelsValues := []string{}
		// if strings.Compare(fieldToAppend, "") == 0 {
		for _, label := range metric.Labels {
			labelsNamesCleaned = append(labelsNamesCleaned, cleanName(label))
			labelsValues = append(labelsValues, row[label])
		}
		// }
		// Construct Prometheus values to sent back
		for metricName, metricHelp := range metric.Help {
			metricType := getMetricType(metricName, metric.Type)
			metricNameCleaned := cleanName(metricName)
			if strings.Compare(metric.FieldToAppend, "") != 0 {
				metricNameCleaned = cleanName(row[metric.FieldToAppend])
			}
			// Dinamic help
			if dinHelpName, exists1 := metric.HelpField[metricName]; exists1 {
				if dinHelpValue, exists2 := row[dinHelpName]; exists2 {
					log.Debugln("	- [DinamicHelp]: Help value '" + metricHelp + "' append with dinamic value '" + dinHelpValue + "'.")
					metricHelp = metricHelp + " " + dinHelpValue
				}
			}
			metricValue := row[metricName]
			// Value mapping
			if metricMap, exists1 := metric.ValueMap[metricName]; exists1 {
				if len(metricMap) > 0 {
					// if mappedValue, exists2 := metricMap[metricValue]; exists2 {
					for key, mappedValue := range metricMap {
						if cleanName(key) == cleanName(metricValue) {
							log.Debugln("	- [ValueMap]: Value '" + metricValue + "' converted to '" + mappedValue + "'.")
							metricValue = mappedValue
							break
						}
					}
					// Add mapping to help
					mappingHelp := " Value mapping: "
					mapStrings := []string{}
					for src, dst := range metricMap {
						mapStrings = append(mapStrings, dst+" - '"+src+"', ")
					}
					sort.Strings(mapStrings)
					for _, mapStr := range mapStrings {
						mappingHelp = mappingHelp + mapStr
					}
					mappingHelp = strings.TrimRight(mappingHelp, ", ")
					mappingHelp = mappingHelp + "."
					metricHelp = metricHelp + mappingHelp
				}
			}
			// If not a float, skip current metric
			metricValueParsed, err := strconv.ParseFloat(metricValue, 64)
			if err != nil {
				log.Errorln("Unable to convert current value to float (metricName='" + metricName + "', value='" + metricValue + "', metricHelp='" + metricHelp + "').")
				continue
			}

			promMetricDesc := prometheus.NewDesc(prometheus.BuildFQName(namespace, metric.Subsystem, metricNameCleaned), metricHelp, labelsNamesCleaned, nil)

			if metricType == prometheus.GaugeValue || metricType == prometheus.CounterValue {
				log.Debugln("	- Converting result looks like: [" + metricNameCleaned + "] : '" + fmt.Sprintf("%g", metricValueParsed) + "'.")
				*ch <- prometheus.MustNewConstMetric(promMetricDesc, metricType, metricValueParsed, labelsValues...)
			} else {
				count, err := strconv.ParseUint(strings.TrimSpace(row["count"]), 10, 64)
				if err != nil {
					log.Errorln("Unable to convert count value to int (metricName='" + metricName + "', value='" + row["count"] + "', metricHelp='" + metricHelp + "').")
					continue
				}
				buckets := make(map[float64]uint64)
				for field, le := range metric.Buckets[metricName] {
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
				*ch <- prometheus.MustNewConstHistogram(promMetricDesc, count, metricValueParsed, buckets, labelsValues...)
			}
			metricsCount++
		}
		return nil
	}

	for _, row := range data {
		// Convert parsed row to Prometheus Metric
		if err := dataRowToPrometheusMetricConverter(row); err != nil {
			return metricsCount, err
		}
	}

	return metricsCount, nil
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

func reloadMetricsIfItChanged(defaultMetricsFile, customMetricsFile string) {
	if checkIfMetricsChanged(customMetricsFile) {
		log.Infoln("Custom metrics changed, reload it...")
		reloadMetrics(defaultMetricsFile, customMetricsFile)
	}
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
