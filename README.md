# Siebel Exporter

[![GoDoc](https://godoc.org/github.com/barkadron/siebel_exporter?status.svg)](http://godoc.org/github.com/barkadron/siebel_exporter)
[![Report card](https://goreportcard.com/badge/github.com/barkadron/siebel_exporter)](https://goreportcard.com/badge/github.com/barkadron/siebel_exporter)

## Description

A [Prometheus](https://prometheus.io/) exporter for [Oracle Siebel CRM](https://www.oracle.com/cx/siebel/). It uses the **srvrmgr** utility to retrieve Siebel status information, parse the output and construct metrics in Prometheus format.

## Installation

### Binary Release

Pre-compiled versions for Linux 64 bit can be found under [releases](https://github.com/barkadron/siebel_exporter/releases).

## Running

Ensure that the environment variable `SRVRMGR_CONNECT_CMD` is set correctly before starting.

> ATTENTION:  
It is imperative that the command contains the `/s` flag indicating a specific server.  
Otherwise, the connection will not be established.  

For example:

```bash
# export Siebel vars:
source /path/to/siebsrvr/siebenv.sh

# export connection command:
export SRVRMGR_CONNECT_CMD="srvrmgr /g sbldevgtw /e SBA_82 /s sbldevapp /u SADMIN /p SADMIN /q"

# run the exporter:
/path/to/binary/siebel_exporter --log.level error
```

## Usage

```text
Usage of siebel_exporter:

    --srvrmgr.connect-command (env: SRVRMGR_CONNECT_CMD)
        Command for connect to srvrmgr.
    
    --srvrmgr.read-buffer-size (env: SRVRMGR_READ_BUFFER_SIZE)
        Size of buffer for reading command output.
        Default: "4096".
    
    --srvrmgr.date-format (env: SRVRMGR_DATE_FORMAT)
        Date format (in GO-style) used by srvrmgr.
        Default: "2006-01-02 15:04:05" (is equal to 'yyyy-mm-dd HH:MM:SS').

    --exporter.default-metrics (env: EXP_DEFAULT_METRICS)
        Path to TOML-file with default metrics.
        Default: "default-metrics.toml".
    
    --exporter.custom-metrics (env: EXP_CUSTOM_METRICS)
        Path to TOML-file that may contain various custom metrics.
    
    --exporter.disable-empty-metrics-override (env: EXP_DISABLE_EMPTY_METRICS_OVERRIDE)
        Disable overriding empty metric values with '0'.
        Default: "false".

    --exporter.disable-extended-metrics (env: EXP_DISABLE_EXTENDED_METRICS)
        Disable metrics with Extended flag.
        Default: "false".

    --web.listen-port (env: WEB_LISTEN_PORT)
        Port to listen on for web interface and metrics.
        Default: "9870".
    
    --web.metrics-endpoint (env: WEB_METRICS_ENDPOINT)
        Path under which to expose metrics.
        Default: "/metrics".
    
    --web.http-read-timeout (env: WEB_HTTP_READ_TIMEOUT)
        Maximum duration for reading the entire request.
        Default: "30".
    
    --web.http-write-timeout (env: WEB_HTTP_WRITE_TIMEOUT)
        Maximum duration before timing out writes of the response.
        Default: "30".
    
    --web.http-idle-timeout (env: WEB_HTTP_IDLE_TIMEOUT)
        Maximum duration to wait for the next request.
        Default: "60".
    
    --web.http-max-requests-in-flight (env: WEB_HTTP_MAX_REQUESTS_IN_FLIGHT)
        Number of concurrent HTTP-requests. Additional requests are responded to with 503 Service Unavailable. If '0', no limit is applied.
        Default: "0".
    
    --web.shutdown-timeout (env: WEB_SHUTDOWN_TIMEOUT)
        Maximum duration to wait for web-server shutdown before forced terminate.
        Default: "3".

    --tls.enable
        Expose metrics using https.
        Default: "false".
    
    --tls.server-cert
        Path to the PEM encoded certificate.
    
    --tls.server-key
        Path to the PEM encoded key.

    --log.level
        Only log messages with the given severity or above. Valid levels: [debug, info, warn, error, fatal].
        Default: "info".

    --log.format
        If set use a syslog logger or JSON logging.
        Default: "stderr".
```

## Default metrics

This exporter comes with a set of default metrics defined in **default-metrics.toml**.

The following metrics are exposed currently:

- Basic:
  - siebel_exporter_last_scrape_duration_seconds  
  - siebel_exporter_last_scrape_error  
  - siebel_exporter_scrape_errors_total
  - siebel_exporter_scrapes_total
  - siebel_gateway_server_up
  - siebel_list_server_sblsrvr_state
  - siebel_list_server_start_time
  - siebel_list_server_end_time
  - siebel_list_compgroup_ca_run_state
  - siebel_list_compgroup_cg_num_components
  - siebel_list_comp_cp_actv_mts_procs
  - siebel_list_comp_cp_disp_run_state
  - siebel_list_comp_cp_end_time
  - siebel_list_comp_cp_max_mts_procs
  - siebel_list_comp_cp_max_tasks
  - siebel_list_comp_cp_num_run_tasks
  - siebel_list_comp_cp_start_time

- Additional:
  - siebel_list_state_values_server_*
  - siebel_list_statistics_server_*

## Custom metrics

If this exporter does not have the metrics you want, you can provide new one using TOML-file.  
To specify this file to the exporter, you can:

- Use `exporter.custom-metrics` option followed by the TOML-file path;
- Export `EXP_CUSTOM_METRICS` variable environment:

  ```bash
    export EXP_CUSTOM_METRICS="path/to/custom-metrics.toml"
  ```

This file must contain the following elements:

- One or several metric section (`[[Metric]]`);
- For each section: `Subsystem`, `Command` and section `[Metric.Help]` with a map between a field of command and a help-string;

Other elements are optional.
