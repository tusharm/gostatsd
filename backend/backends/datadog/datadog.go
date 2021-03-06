package datadog

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strings"
	"time"

	backendTypes "github.com/atlassian/gostatsd/backend/types"
	"github.com/atlassian/gostatsd/types"

	log "github.com/Sirupsen/logrus"
	"github.com/cenkalti/backoff"
	"github.com/spf13/viper"
	"golang.org/x/net/context"
)

const (
	apiURL = "https://app.datadoghq.com"
	// BackendName is the name of this backend.
	BackendName                  = "datadog"
	dogstatsdVersion             = "5.6.3"
	dogstatsdUserAgent           = "python-requests/2.6.0 CPython/2.7.10"
	defaultMaxRequestElapsedTime = 15 * time.Second
	defaultClientTimeout         = 9 * time.Second
	// defaultMetricsPerBatch is the default number of metrics to send in a single batch.
	defaultMetricsPerBatch = 1000
	// maxResponseSize is the maximum response size we are willing to read.
	maxResponseSize = 10 * 1024
)

// client represents a Datadog client.
type client struct {
	apiKey                string
	apiEndpoint           string
	maxRequestElapsedTime time.Duration
	client                http.Client
	metricsPerBatch       uint
	now                   func() time.Time // Returns current time. Useful for testing.
}

const sampleConfig = `
[datadog]
	## Datadog API key
	api_key = "my-secret-key" # required.

	## Connection timeout.
	# timeout = "5s"
`

// event represents an event data structure for Datadog.
type event struct {
	Title          string   `json:"title"`
	Text           string   `json:"text"`
	DateHappened   int64    `json:"date_happened,omitempty"`
	Hostname       string   `json:"host,omitempty"`
	AggregationKey string   `json:"aggregation_key,omitempty"`
	SourceTypeName string   `json:"source_type_name,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	Priority       string   `json:"priority,omitempty"`
	AlertType      string   `json:"alert_type,omitempty"`
}

// SendMetricsAsync flushes the metrics to Datadog, preparing payload synchronously but doing the send asynchronously.
func (d *client) SendMetricsAsync(ctx context.Context, metrics *types.MetricMap, cb backendTypes.SendCallback) {
	if metrics.NumStats == 0 {
		cb(nil)
		return
	}
	counter := 0
	results := make(chan error)
	d.processMetrics(metrics, func(ts *timeSeries) {
		go func() {
			err := d.postMetrics(ts)
			select {
			case <-ctx.Done():
			case results <- err:
			}
		}()
		counter++
	})
	go func() {
		errs := make([]error, 0, counter)
	loop:
		for c := 0; c < counter; c++ {
			select {
			case <-ctx.Done():
				errs = append(errs, ctx.Err())
				break loop
			case err := <-results:
				errs = append(errs, err)
			}
		}
		cb(errs)
	}()
}

func (d *client) processMetrics(metrics *types.MetricMap, cb func(*timeSeries)) {
	fl := flush{
		ts: &timeSeries{
			Series: make([]metric, 0, d.metricsPerBatch),
		},
		timestamp:        float64(d.now().Unix()),
		flushIntervalSec: metrics.FlushInterval.Seconds(),
		metricsPerBatch:  d.metricsPerBatch,
		cb:               cb,
	}

	metrics.Counters.Each(func(key, tagsKey string, counter types.Counter) {
		fl.addMetric(key, rate, counter.PerSecond, counter.Hostname, counter.Tags)
		fl.addMetric(fmt.Sprintf("%s.count", key), gauge, float64(counter.Value), counter.Hostname, counter.Tags)
		fl.maybeFlush()
	})

	metrics.Timers.Each(func(key, tagsKey string, timer types.Timer) {
		fl.addMetric(fmt.Sprintf("%s.lower", key), gauge, timer.Min, timer.Hostname, timer.Tags)
		fl.addMetric(fmt.Sprintf("%s.upper", key), gauge, timer.Max, timer.Hostname, timer.Tags)
		fl.addMetric(fmt.Sprintf("%s.count", key), gauge, float64(timer.Count), timer.Hostname, timer.Tags)
		fl.addMetric(fmt.Sprintf("%s.count_ps", key), rate, timer.PerSecond, timer.Hostname, timer.Tags)
		fl.addMetric(fmt.Sprintf("%s.mean", key), gauge, timer.Mean, timer.Hostname, timer.Tags)
		fl.addMetric(fmt.Sprintf("%s.median", key), gauge, timer.Median, timer.Hostname, timer.Tags)
		fl.addMetric(fmt.Sprintf("%s.std", key), gauge, timer.StdDev, timer.Hostname, timer.Tags)
		fl.addMetric(fmt.Sprintf("%s.sum", key), gauge, timer.Sum, timer.Hostname, timer.Tags)
		fl.addMetric(fmt.Sprintf("%s.sum_squares", key), gauge, timer.SumSquares, timer.Hostname, timer.Tags)
		for _, pct := range timer.Percentiles {
			fl.addMetric(fmt.Sprintf("%s.%s", key, pct.Str), gauge, pct.Float, timer.Hostname, timer.Tags)
		}
		fl.maybeFlush()
	})

	metrics.Gauges.Each(func(key, tagsKey string, g types.Gauge) {
		fl.addMetric(key, gauge, g.Value, g.Hostname, g.Tags)
		fl.maybeFlush()
	})

	metrics.Sets.Each(func(key, tagsKey string, set types.Set) {
		fl.addMetric(key, gauge, float64(len(set.Values)), set.Hostname, set.Tags)
		fl.maybeFlush()
	})

	fl.finish()
}

func (d *client) postMetrics(ts *timeSeries) error {
	return d.post("/api/v1/series", "metrics", ts)
}

// SendEvent sends an event to Datadog.
func (d *client) SendEvent(ctx context.Context, e *types.Event) error {
	return d.post("/api/v1/events", "events", &event{
		Title:          e.Title,
		Text:           e.Text,
		DateHappened:   e.DateHappened,
		Hostname:       e.Hostname,
		AggregationKey: e.AggregationKey,
		SourceTypeName: e.SourceTypeName,
		Tags:           e.Tags,
		Priority:       e.Priority.StringWithEmptyDefault(),
		AlertType:      e.AlertType.StringWithEmptyDefault(),
	})
}

// SampleConfig returns the sample config for the datadog backend.
func (d *client) SampleConfig() string {
	return sampleConfig
}

// BackendName returns the name of the backend.
func (d *client) BackendName() string {
	return BackendName
}

func (d *client) post(path, typeOfPost string, data interface{}) error {
	tsBytes, err := json.Marshal(data)
	if err != nil {
		return fmt.Errorf("[%s] unable to marshal %s: %v", BackendName, typeOfPost, err)
	}
	log.Debugf("[%s] %s json: %s", BackendName, typeOfPost, tsBytes)

	b := backoff.NewExponentialBackOff()
	b.MaxElapsedTime = d.maxRequestElapsedTime
	err = backoff.RetryNotify(d.doPost(path, tsBytes), b, func(err error, d time.Duration) {
		log.Warnf("[%s] failed to send %s, sleeping for %s: %v", BackendName, typeOfPost, d, err)
	})
	if err != nil {
		return fmt.Errorf("[%s] %v", BackendName, err)
	}

	return nil
}

func (d *client) doPost(path string, body []byte) backoff.Operation {
	authenticatedURL := d.authenticatedURL(path)
	return func() error {
		req, err := http.NewRequest("POST", authenticatedURL, bytes.NewReader(body))
		if err != nil {
			return fmt.Errorf("unable to create http.Request: %v", err)
		}
		req.Header.Add("Content-Type", "application/json")
		// Mimic dogstatsd code
		req.Header.Add("DD-Dogstatsd-Version", dogstatsdVersion)
		req.Header.Add("User-Agent", dogstatsdUserAgent)
		resp, err := d.client.Do(req)
		if err != nil {
			return fmt.Errorf("error POSTing: %s", strings.Replace(err.Error(), d.apiKey, "*****", -1))
		}
		defer resp.Body.Close()
		body := io.LimitReader(resp.Body, maxResponseSize)
		if resp.StatusCode < http.StatusOK || resp.StatusCode > http.StatusNoContent {
			b, _ := ioutil.ReadAll(body)
			log.Infof("[%s] failed request status: %d\n%s", BackendName, resp.StatusCode, b)
			return fmt.Errorf("received bad status code %d", resp.StatusCode)
		}
		_, _ = io.Copy(ioutil.Discard, body)
		return nil
	}
}

func (d *client) authenticatedURL(path string) string {
	q := url.Values{
		"api_key": []string{d.apiKey},
	}
	return fmt.Sprintf("%s%s?%s", d.apiEndpoint, path, q.Encode())
}

// NewClientFromViper returns a new Datadog API client.
func NewClientFromViper(v *viper.Viper) (backendTypes.Backend, error) {
	dd := getSubViper(v, "datadog")
	dd.SetDefault("api_endpoint", apiURL)
	dd.SetDefault("metrics_per_batch", defaultMetricsPerBatch)
	dd.SetDefault("timeout", defaultClientTimeout)
	dd.SetDefault("max_request_elapsed_time", defaultMaxRequestElapsedTime)
	return NewClient(
		dd.GetString("api_endpoint"),
		dd.GetString("api_key"),
		uint(dd.GetInt("metrics_per_batch")),
		dd.GetDuration("timeout"),
		dd.GetDuration("max_request_elapsed_time"),
	)
}

// NewClient returns a new Datadog API client.
func NewClient(apiEndpoint, apiKey string, metricsPerBatch uint, clientTimeout, maxRequestElapsedTime time.Duration) (backendTypes.Backend, error) {
	if apiEndpoint == "" {
		return nil, fmt.Errorf("[%s] apiEndpoint is required", BackendName)
	}
	if apiKey == "" {
		return nil, fmt.Errorf("[%s] apiKey is required", BackendName)
	}
	if metricsPerBatch <= 0 {
		return nil, fmt.Errorf("[%s] metricsPerBatch must be positive", BackendName)
	}
	if clientTimeout <= 0 {
		return nil, fmt.Errorf("[%s] clientTimeout must be positive", BackendName)
	}
	if maxRequestElapsedTime <= 0 {
		return nil, fmt.Errorf("[%s] maxRequestElapsedTime must be positive", BackendName)
	}
	log.Infof("[%s] maxRequestElapsedTime=%s clientTimeout=%s metricsPerBatch=%d", BackendName, maxRequestElapsedTime, clientTimeout, metricsPerBatch)
	return &client{
		apiKey:                apiKey,
		apiEndpoint:           apiEndpoint,
		maxRequestElapsedTime: maxRequestElapsedTime,
		client: http.Client{
			Timeout: clientTimeout,
		},
		metricsPerBatch: metricsPerBatch,
		now:             time.Now,
	}, nil
}

// Workaround https://github.com/spf13/viper/pull/165 and https://github.com/spf13/viper/issues/191
func getSubViper(v *viper.Viper, key string) *viper.Viper {
	var n *viper.Viper
	namespace := v.Get(key)
	if namespace != nil {
		n = v.Sub(key)
	}
	if n == nil {
		n = viper.New()
	}
	return n
}
