package statsd

import (
	"bytes"
	"fmt"
	"io"
	"log"
	"net"
	"strconv"
)

// DefaultMetricsAddr is the default address on which a MetricReceiver will listen
const DefaultMetricsAddr = ":8125"

// Objects implementing the Handler interface can be used to handle metrics for a MetricReceiver
type Handler interface {
	HandleMetric(m Metric)
}

// The HandlerFunc type is an adapter to allow the use of ordinary functions as metric handlers
type HandlerFunc func(Metric)

// HandleMetric calls f(m)
func (f HandlerFunc) HandleMetric(m Metric) {
	f(m)
}

// MetricReceiver receives data on its listening port and converts lines in to Metrics.
// For each Metric it calls r.Handler.HandleMetric()
type MetricReceiver struct {
	Addr    string  // UDP address on which to listen for metrics
	Handler Handler // handler to invoke
}

// ListenAndReceive listens on the UDP network address of srv.Addr and then calls
// Receive to handle the incoming datagrams. If Addr is blank then DefaultMetricsAddr is used.
func (r *MetricReceiver) ListenAndReceive() error {
	addr := r.Addr
	if addr == "" {
		addr = DefaultMetricsAddr
	}
	c, err := net.ListenPacket("udp", addr)
	if err != nil {
		return err
	}
	return r.Receive(c)
}

// Receive accepts incoming datagrams on c and calls r.Handler.HandleMetric() for each line in the
// datagram that successfully parses in to a Metric
func (r *MetricReceiver) Receive(c net.PacketConn) error {
	defer c.Close()

	msg := make([]byte, 1024)
	for {
		nbytes, _, err := c.ReadFrom(msg)
		if err != nil {
			log.Printf("%s", err)
			continue
		}
		go r.handleMessage(msg[:nbytes])
	}
	panic("not reached")
}

// handleMessage handles the contents of a datagram and attempts to parse a Metric from each line
func (srv *MetricReceiver) handleMessage(msg []byte) {
	buf := bytes.NewBuffer(msg)
	for {
		line, err := buf.ReadBytes('\n')
		if err == io.EOF {
			break
		}
		if err != nil {
			log.Printf("error reading message: %s", err)
			return
		}

		lineLength := len(line)
		// Only process lines with more than one character
		if lineLength > 1 {
			metric, err := parseLine(line[:lineLength-1])
			if err != nil {
				log.Println(line)
				log.Println(err)
				continue
			}
			go srv.Handler.HandleMetric(metric)
		}
	}
}

func parseLine(line []byte) (Metric, error) {
	var metric Metric

	buf := bytes.NewBuffer(line)
	bucket, err := buf.ReadBytes(':')
	if err != nil {
		fmt.Println(line)
		return metric, fmt.Errorf("error parsing metric name: %s", err)
	}
	metric.Bucket = string(bucket[:len(bucket)-1])

	value, err := buf.ReadBytes('|')
	if err != nil {
		return metric, fmt.Errorf("error parsing metric value: %s", err)
	}
	metric.Value, err = strconv.ParseFloat(string(value[:len(value)-1]), 64)
	if err != nil {
		return metric, fmt.Errorf("error converting metric value: %s", err)
	}

	metricType := buf.Bytes()
	if err != nil && err != io.EOF {
		return metric, fmt.Errorf("error parsing metric type: %s", err)
	}

	switch string(metricType[:len(metricType)]) {
	case "ms":
		// Timer
		metric.Type = TIMER
	case "g":
		// Gauge
		metric.Type = GAUGE
	case "c":
		metric.Type = COUNTER
	default:
		err = fmt.Errorf("invalid metric type: %q", metricType)
		return metric, err
	}

	return metric, nil
}