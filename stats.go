package ddstats

import (
	"fmt"
	"hash/fnv"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/jmizell/ddstats/client"
)

type job struct {
	metric   *metric
	shutdown bool
	flush    bool
}

type Stats struct {
	namespace       string
	host            string
	tags            []string
	flushInterval   time.Duration
	workerCount     int
	workerBuffer    int
	metricBuffer    int
	client          client.APIClient
	metrics         []map[string]*metric
	metricsQueue    []*client.DDMetric
	metricQueueLock *sync.Mutex
	jobs            chan *job
	workers         []chan *job
	shutdown        bool
	shutdownLock    *sync.Mutex
	workerWG        *sync.WaitGroup
	flushWG         *sync.WaitGroup
	ready           chan bool
	flushCallback   func(metricSeries []*client.DDMetric)
	errorCallback   func(err error, metricSeries []*client.DDMetric)
	errors          []error
	maxErrors       int
	errorLock       *sync.RWMutex
	dropped         uint64
	lastFlush       time.Time
}

func NewStats(cfg *Config) (*Stats, error) {

	s := &Stats{
		namespace:     cfg.Namespace,
		host:          cfg.Host,
		tags:          cfg.Tags,
		flushInterval: time.Duration(cfg.FlushIntervalSeconds) * time.Second,
		workerCount:   cfg.WorkerCount,
		workerBuffer:  cfg.WorkerBuffer,
		metricBuffer:  cfg.MetricBuffer,
		maxErrors:     cfg.MaxErrors,
		ready:         make(chan bool, 1),
	}

	if cfg.client != nil {
		s.client = cfg.client
	} else if cfg.APIKey != "" {
		s.client = client.NewDDClient(cfg.APIKey)
	} else {
		return nil, fmt.Errorf("no client configured")
	}

	go s.start()
	s.blockReady()
	return s, nil
}

func (c *Stats) start() {

	// Setup our channels
	c.shutdownLock = &sync.Mutex{}
	c.shutdown = false
	c.jobs = make(chan *job, c.metricBuffer)

	// Setup wait group for workers. Flush wait group is separate as
	// we don't want to block processing new stats, if a flush worker
	// is running slow.
	c.workerWG = &sync.WaitGroup{}
	c.flushWG = &sync.WaitGroup{}

	// Here we're tracking our errors
	c.errors = []error{}
	c.errorLock = &sync.RWMutex{}

	// Setup our slice of map metrics. There is a separate map for each worker
	// so we can avoid locking on storing metrics. This will be zeroed out at
	// each flush cycle.
	c.metrics = make([]map[string]*metric, c.workerCount)
	for i := range c.metrics {
		c.metrics[i] = map[string]*metric{}
	}

	// Setup our raw metrics publish queue
	c.metricsQueue = make([]*client.DDMetric, 0)
	c.metricQueueLock = &sync.Mutex{}

	// Start our works, each worker has it's own channel.
	c.workers = make([]chan *job, c.workerCount)
	for i := 0; i < c.workerCount; i++ {
		c.workers[i] = make(chan *job, c.workerBuffer)
		go c.worker(c.workers[i], i)
	}

	// Start the flush worker. This will send a flush signal until given
	// a shutdown signal.
	shutdownFlushSignalWorker := make(chan bool)
	flushSignalWorkerWG := &sync.WaitGroup{}
	flushSignalWorkerWG.Add(1)
	go func() {
		defer flushSignalWorkerWG.Done()
		flush := time.NewTicker(c.flushInterval)
		for {
			select {
			case <-flush.C:
				// Add a job to the flush wait group
				c.flushWG.Add(1)
				c.jobs <- &job{flush: true}
			case <-shutdownFlushSignalWorker:
				flush.Stop()
				return
			}
		}
	}()
	c.ready <- true

	// We need to track time between flushes. If a flush is called before the scheduled
	// interval, we will need to know exactly how much time has passed, so we can calculate
	// our rate metrics.
	c.lastFlush = time.Now()
	for {
		j, ok := <-c.jobs
		if !ok {
			return
		}

		switch {
		case j.shutdown:

			// Perform a final flush of all stats. Anything buffered in the updates channel
			// will be dropped.
			c.commitFlush()

			// On shutdown, we'll signal all the workers to exit after completing the current job
			for i := range c.workers {
				c.workerWG.Add(1)
				c.workers[i] <- &job{shutdown: true}
			}

			// Signal to the flush worker to shutdown, wait before returning
			shutdownFlushSignalWorker <- true
			flushSignalWorkerWG.Wait()

			// Wait for all workers, and flush to complete
			c.workerWG.Wait()
			c.flushWG.Wait()

			return
		case j.flush:
			// Copy out the metrics for this interval, and send them
			c.commitFlush()
		case j.metric != nil:
			// New metric has been sent, we want to add a job to the wait group, and
			// then we assign it to the worker by using a FNV-1a hash. This should ensure
			// that the same worker always sees the same metric.
			c.workerWG.Add(1)
			c.workers[fnv1a(j.metric.name)%uint32(len(c.workers))] <- j
		}
	}
}

func (c *Stats) commitFlush() {

	// On a flush signal we need to wait for all current metrics to be processed
	// by the workers
	c.workerWG.Wait()

	// We need to make a copy of all the metrics to a new data structure
	flattenedMetrics := make(map[string]*metric)
	for _, m := range c.metrics {
		for k, v := range m {
			flattenedMetrics[k] = v
		}
	}

	// Then we zero out all of the metrics, and start with new values for the
	// next flush interval.
	for i := range c.metrics {
		c.metrics[i] = map[string]*metric{}
	}

	// Update the flush interval, and send the metrics to the flush worker.
	interval := time.Since(c.lastFlush)
	go c.send(flattenedMetrics, interval)
	c.lastFlush = time.Now()
}

func (c *Stats) blockReady() {
	<-c.ready
}

func (c *Stats) worker(jobs chan *job, id int) {
	for {
		job := <-jobs
		if job.shutdown {
			c.workerWG.Done()
			return
		}

		// Metrics are indexed by a combination of the metric name, and the list
		// of tags. Order of the tags sent to the job shouldn't matter, as we
		// sort them, before creating the index key.
		key := metricKey(job.metric.name, job.metric.tags)

		// Store or update the metric
		if _, ok := c.metrics[id][key]; ok {
			c.metrics[id][key].update(job.metric.value)
		} else {
			c.metrics[id][key] = job.metric
		}

		// Signalling done, allows us to track if any jobs are being worked on,
		// in order for us to avoid concurrent access to the metrics map on a
		// flush operation.
		c.workerWG.Done()
	}
}

func (c *Stats) send(metrics map[string]*metric, flushTime time.Duration) {

	defer c.flushWG.Done()

	var metricsQueue []*client.DDMetric
	c.metricQueueLock.Lock()
	if len(c.metricsQueue) > 0 {
		metricsQueue = c.metricsQueue
		c.metricsQueue = make([]*client.DDMetric, 0)
	}
	c.metricQueueLock.Unlock()
	if len(metrics) == 0 && metricsQueue == nil {
		return
	}

	// Allocate our new series, and copy the metric queue
	var metricsSeries []*client.DDMetric
	if metricsQueue != nil {
		metricsSeries = make([]*client.DDMetric, 0, len(metrics)+len(metricsQueue))
		for _, m := range metricsQueue {
			metricsSeries = append(metricsSeries, m)
		}
	} else {
		metricsSeries = make([]*client.DDMetric, 0, len(metrics))
	}
	for _, m := range metrics {
		metricsSeries = append(metricsSeries, m.getMetric(c.namespace, c.host, c.tags, flushTime))
	}

	if err := c.SendSeries(metricsSeries); err != nil {
		c.errorLock.Lock()
		c.errors = appendErrorsList(c.errors, err, c.maxErrors)
		c.errorLock.Unlock()
		if c.errorCallback != nil {
			c.errorCallback(err, metricsSeries)
		}
	}

	if c.flushCallback != nil {
		c.flushCallback(metricsSeries)
	}
}

// SendSeries immediately posts an DDMetric series to the Datadog api. Each metric in the series
// is checked for an host name, and the correct namespace. If host, or namespace vales are missing,
// the values will be filled before sending to the api. Global tags are added to all metrics.
func (c *Stats) SendSeries(series []*client.DDMetric) error {
	for _, m := range series {
		if m.Host == "" {
			m.Host = c.host
		}
		m.Metric = prependNamespace(c.namespace, m.Metric)
		m.Tags = combineTags(c.tags, m.Tags)
	}
	return c.client.SendSeries(&client.DDMetricSeries{Series: series})
}

// QueueSeries adds a series of metrics to the queue to be be sent with the next flush.
func (c *Stats) QueueSeries(series []*client.DDMetric) {
	for _, m := range series {
		if m.Host == "" {
			m.Host = c.host
		}
		m.Metric = prependNamespace(c.namespace, m.Metric)
		m.Tags = combineTags(c.tags, m.Tags)
	}
	c.metricQueueLock.Lock()
	defer c.metricQueueLock.Unlock()
	c.metricsQueue = append(c.metricsQueue, series...)
}

// ServiceCheck immediately posts an DDServiceCheck to he Datadog api. The namespace is
// prepended to the check name, if it is missing. Host, and time is automatically added.
// Global tags are appended to tags passed to the method.
func (c *Stats) ServiceCheck(check, message string, status client.Status, tags []string) error {
	return c.client.SendServiceCheck(&client.DDServiceCheck{
		Check:     prependNamespace(c.namespace, check),
		Hostname:  c.host,
		Message:   message,
		Status:    status,
		Tags:      combineTags(c.tags, tags),
		Timestamp: time.Now().Unix(),
	})
}

// Event immediately posts an DDEvent to he Datadog api. If host, or namespace vales are missing,
// the values will be filled before sending to the api. Global tags are appended to the event.
func (c *Stats) Event(event *client.DDEvent) error {
	if event.Host == "" {
		event.Host = c.host
	}
	if event.DateHappened == 0 {
		event.DateHappened = time.Now().Unix()
	}
	event.AggregationKey = prependNamespace(c.namespace, event.AggregationKey)
	event.Tags = combineTags(c.tags, event.Tags)
	return c.client.SendEvent(event)
}

// Increment creates or increments a count metric by +1. This is a non-blocking method, if
// the channel buffer is full, then the metric is not recorded. Count stats are sent as count,
// by taking the sum value of all values in the flush interval.
func (c *Stats) Increment(name string, tags []string) {
	c.Count(name, 1, tags)
}

// Decrement creates or subtracts a count metric by -1. This is a non-blocking method, if
// the channel buffer is full, then the metric is not recorded. Count stats are sent as count,
// by taking the sum value of all values in the flush interval.
func (c *Stats) Decrement(name string, tags []string) {
	c.Count(name, -1, tags)
}

// Count creates or adds a count metric by value. This is a non-blocking method, if
// the channel buffer is full, then the metric is not recorded. Count stats are sent as count,
// by taking the sum value of all values in the flush interval.
func (c *Stats) Count(name string, value float64, tags []string) {
	select {
	case c.jobs <- &job{metric: &metric{
		name:  name,
		class: client.Count,
		value: value,
		tags:  tags,
	}}:
	default:
		atomic.AddUint64(&c.dropped, 1)
	}
}

// IncrementRate creates or increments a rate metric by +1. This is a non-blocking method, if
// the channel buffer is full, then the metric is not recorded. Rate stats are sent as rate,
// by taking the count value and dividing by the number of seconds since the last flush.
func (c *Stats) IncrementRate(name string, tags []string) {
	c.Rate(name, 1, tags)
}

// DecrementRate creates or subtracts a rate metric by -1. This is a non-blocking method, if
// the channel buffer is full, then the metric is not recorded. Rate stats are sent as rate,
// by taking the count value and dividing by the number of seconds since the last flush.
func (c *Stats) DecrementRate(name string, tags []string) {
	c.Rate(name, -1, tags)
}

// Rate creates or adds a rate metric by value. This is a non-blocking method, if
// the channel buffer is full, then the metric is not recorded. Rate stats are sent as rate,
// by taking the count value and dividing by the number of seconds since the last flush.
func (c *Stats) Rate(name string, value float64, tags []string) {
	select {
	case c.jobs <- &job{metric: &metric{
		name:  name,
		class: client.Rate,
		value: value,
		tags:  tags,
	}}:
	default:
		atomic.AddUint64(&c.dropped, 1)
	}
}

// Gauge creates or updates a gauge metric by value. This is a non-blocking method, if
// the channel buffer is full, then the metric not recorded. Gauge stats are reported
// as the last value sent before flush is called.
func (c *Stats) Gauge(name string, value float64, tags []string) {
	select {
	case c.jobs <- &job{metric: &metric{
		name:  name,
		class: client.Gauge,
		value: value,
		tags:  tags,
	}}:
	default:
		atomic.AddUint64(&c.dropped, 1)
	}
}

// GetDroppedMetricCount returns the number off metrics submitted to the metric queue,
// and where dropped because the queue was full.
func (c *Stats) GetDroppedMetricCount() uint64 {
	return atomic.LoadUint64(&c.dropped)
}

// Flush signals the main worker thread to copy all current metrics, and send them
// to the Datadog api. Flush blocks until all flush jobs complete.
// been sent, use FlushWait.
func (c *Stats) Flush() {
	// Add a job to the flush wait group
	c.flushWG.Add(1)
	c.jobs <- &job{flush: true}
	c.flushWG.Wait()
}

// FlushCallback registers a call back function that will be called at the end of every successful flush.
func (c *Stats) FlushCallback(f func(metricSeries []*client.DDMetric)) {
	c.flushCallback = f
}

// ErrorCallback registers a call back function that will be called if any error is returned
// by the api client during a flush.
func (c *Stats) ErrorCallback(f func(err error, metricSeries []*client.DDMetric)) {
	c.errorCallback = f
}

// Errors returns a slice of all errors returned by the api client during a flush.
func (c *Stats) Errors() []error {
	c.errorLock.RLock()
	defer c.errorLock.RUnlock()
	errs := c.errors
	return errs
}

// Close signals a shutdown, and blocks while waiting for flush to complete, and all workers to shutdown.
func (c *Stats) Close() {

	c.shutdownLock.Lock()
	defer c.shutdownLock.Unlock()
	if c.shutdown {
		return
	}

	c.shutdown = true
	c.flushWG.Add(1)
	c.jobs <- &job{shutdown: true}
	c.workerWG.Wait()
	c.flushWG.Wait()
}

func prependNamespace(namespace, name string) string {

	if namespace == "" || strings.HasPrefix(name, namespace) {
		return name
	}

	return fmt.Sprintf("%s.%s", namespace, name)
}

func combineTags(tags1, tags2 []string) []string {

	if tags1 == nil && tags2 == nil {
		return []string{}
	} else if tags1 == nil {
		return tags2
	} else if tags2 == nil {
		return tags1
	}

	// Tags should be unique, duplicate tags should be filtered
	// out of the list
	uniqueTags := make(map[string]bool)
	for _, tag := range append(tags1, tags2...) {
		uniqueTags[tag] = true
	}

	newTags := make([]string, 0, len(uniqueTags))
	for tag := range uniqueTags {
		newTags = append(newTags, tag)
	}

	return newTags
}

func metricKey(name string, tags []string) string {

	// We have to sort the tags, in order to generate a consistent key.
	// If a user swaps the order of a key, we don't want to store that
	// metric as new metric.
	sort.Strings(tags)
	return fmt.Sprintf("%s%s", name, strings.Join(tags, ""))
}

func fnv1a(v string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(v))
	return h.Sum32()
}

func appendErrorsList(errors []error, err error, max int) []error {

	if len(errors) >= max {
		errors = errors[1:]
	}

	return append(errors, err)
}
