// Copyright © 2017 Circonus, Inc. <support@circonus.com>
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.
//

package statsd

import (
	"bytes"
	stdlog "log"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/circonus-labs/circonus-agent/internal/config"
	cgm "github.com/circonus-labs/circonus-gometrics"
	"github.com/pkg/errors"
	"github.com/rs/zerolog"
	"github.com/rs/zerolog/log"
	"github.com/spf13/viper"
)

// Server defines a statsd server
type Server struct {
	hostMetrics           *cgm.CirconusMetrics
	hostMetricsmu         sync.Mutex
	groupMetrics          *cgm.CirconusMetrics
	groupMetricsmu        sync.Mutex
	logger                zerolog.Logger
	hostPrefix            string
	hostCategory          string
	groupPrefix           string
	groupCounterOp        string
	groupGaugeOp          string
	groupSetOp            string
	metricRegex           *regexp.Regexp
	metricRegexGroupNames []string
}

const (
	maxPacketSize   = 1472
	packetQueueSize = 1000
	destHost        = "host"
	destGroup       = "group"
	destIgnore      = "ignore"
)

// New returns a statsd server definition
func New() *Server {
	if viper.GetBool(config.KeyStatsdDisabled) {
		log.Info().Msg("StatsD disabled, not starting listener")
		return nil
	}

	s := Server{
		logger:         log.With().Str("pkg", "statsd").Logger(),
		hostPrefix:     viper.GetString(config.KeyStatsdHostPrefix),
		hostCategory:   viper.GetString(config.KeyStatsdHostCategory),
		groupPrefix:    viper.GetString(config.KeyStatsdGroupPrefix),
		groupCounterOp: viper.GetString(config.KeyStatsdGroupCounters),
		groupGaugeOp:   viper.GetString(config.KeyStatsdGroupGauges),
		groupSetOp:     viper.GetString(config.KeyStatsdGroupSets),
	}

	s.metricRegex = regexp.MustCompile("^(?P<name>[^:\\s]+):(?P<value>[^|\\s]+)\\|(?P<type>[a-z]+)(?:@(?P<sample>[0-9.]+))?$")
	s.metricRegexGroupNames = s.metricRegex.SubexpNames()

	return &s
}

// Start the StatsD listener
func (s *Server) Start() error {

	if err := s.initHostMetrics(); err != nil {
		return errors.Wrap(err, "Initializing host metrics for StatsD")
	}

	if err := s.initGroupMetrics(); err != nil {
		return errors.Wrap(err, "Initializing group metrics for StatsD")
	}

	packetQueue := make(chan []byte, packetQueueSize)
	ec := make(chan error)

	address := net.JoinHostPort("localhost", viper.GetString(config.KeyStatsdPort))
	addr, err := net.ResolveUDPAddr("udp", address)
	if err != nil {
		return errors.Wrapf(err, "resolving address '%s'", address)
	}

	s.logger.Info().Str("addr", addr.String()).Msg("StatsD listener")

	listener, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}

	// run the listener
	go func() {
		defer listener.Close()

		for {
			buff := make([]byte, maxPacketSize)
			n, err := listener.Read(buff)
			if err != nil {
				ec <- err
				return
			}
			pkt := make([]byte, n)
			copy(pkt, buff[:n])
			packetQueue <- pkt
		}
	}()

	// run the packet handler separately so packet processing
	// does not block the listener
	go func() {
		for {
			select {
			case pkt := <-packetQueue:
				s.logger.Debug().Str("packet", string(pkt)).Msg("received")
				metrics := bytes.Split(pkt, []byte("\n"))
				for _, metric := range metrics {
					if err := s.parseMetric(string(metric)); err != nil {
						s.logger.Warn().Err(err).Str("metric", string(metric)).Msg("parsing")
					}
				}
			}
		}
	}()

	// block until an error [from the server portion] is recieved or some other component exits
	return <-ec
}

// Flush *host* metrics only
// NOTE: group metrics flush independently via cgm
func (s *Server) Flush() *cgm.Metrics {
	if viper.GetBool(config.KeyStatsdDisabled) {
		return nil
	}
	if s.hostMetrics == nil {
		return &cgm.Metrics{}
	}
	s.hostMetricsmu.Lock()
	defer s.hostMetricsmu.Unlock()
	return s.hostMetrics.FlushMetrics()
}

// getMetricDestination determines "where" a metric should be sent (host or group)
// and cleans up the metric name if it matches a host|group prefix
func (s *Server) getMetricDestination(metricName string) (string, string) {
	if s.hostPrefix == "" && s.groupPrefix == "" { // no host/group prefixes - send all metrics to host
		return destHost, metricName
	}

	if s.hostPrefix != "" && s.groupPrefix != "" { // explicit host and group, otherwise ignore
		if strings.HasPrefix(metricName, s.hostPrefix) {
			return destHost, strings.Replace(metricName, s.hostPrefix, "", 1)
		}
		if strings.HasPrefix(metricName, s.groupPrefix) {
			return destGroup, strings.Replace(metricName, s.groupPrefix, "", 1)
		}
		log.Debug().Str("metric_name", metricName).Msg("does not match host|group prefix, ignoring")
		return destIgnore, metricName
	}

	if s.groupPrefix != "" && s.hostPrefix == "" { // default to host
		if strings.HasPrefix(metricName, s.groupPrefix) {
			return destGroup, strings.Replace(metricName, s.groupPrefix, "", 1)
		}
		return destHost, metricName
	}

	if s.groupPrefix == "" && s.hostPrefix != "" { // default to group
		if strings.HasPrefix(metricName, s.hostPrefix) {
			return destHost, strings.Replace(metricName, s.hostPrefix, "", 1)
		}
		return destGroup, metricName
	}

	s.logger.Debug().Str("metric_name", metricName).Msg("does not match host|group criteria, ignoring")
	return destIgnore, metricName
}

func (s *Server) parseMetric(metric string) error {
	// ignore 'blank' lines/empty strings
	if len(metric) == 0 {
		return nil
	}

	metricName := ""
	metricType := ""
	metricValue := ""
	metricRate := ""
	sampleRate := 0.0

	if !s.metricRegex.MatchString(metric) {
		return errors.Errorf("invalid metric format '%s', ignoring", metric)
	}

	for _, match := range s.metricRegex.FindAllStringSubmatch(metric, -1) {
		for gIdx, matchVal := range match {
			switch s.metricRegexGroupNames[gIdx] {
			case "name":
				metricName = matchVal
			case "type":
				metricType = matchVal
			case "value":
				metricValue = matchVal
			case "sample":
				metricRate = matchVal
			default:
				// ignore any other groups
			}
		}
	}

	if metricName == "" {
		return errors.New("invalid metric name (empty), ignoring")
	}

	if metricValue == "" {
		return errors.New("invalid metric value (empty), ignoring")
	}

	if metricRate != "" {
		r, err := strconv.ParseFloat(metricRate, 32)
		if err != nil {
			return errors.Errorf("invalid metric sampling rate (%s), ignoring", err)
		}
		sampleRate = r
	}

	var (
		dest       *cgm.CirconusMetrics
		metricDest string
	)
	metricDest, metricName = s.getMetricDestination(metricName)

	if metricDest == destGroup {
		dest = s.groupMetrics
	} else if metricDest == destHost {
		dest = s.hostMetrics
	}

	if dest == nil {
		return errors.Errorf("invalid metric destination (%s)->(%s)", metric, metricDest)
	}

	switch metricType {
	case "c": // counter
		v, err := strconv.ParseUint(metricValue, 10, 64)
		if err != nil {
			return errors.Wrap(err, "invalid counter value")
		}
		if v == 0 {
			v = 1
		}
		if sampleRate > 0 {
			v = uint64(float64(v) * (1 / sampleRate))
		}
		dest.IncrementByValue(metricName, v)
	case "g": // gauge
		if strings.Contains(metricValue, ".") {
			v, err := strconv.ParseFloat(metricValue, 64)
			if err != nil {
				return errors.Wrap(err, "invalid gauge value")
			}
			dest.Gauge(metricName, v)
		} else if strings.Contains(metricValue, "-") {
			v, err := strconv.ParseInt(metricValue, 10, 64)
			if err != nil {
				return errors.Wrap(err, "invalid gauge value")
			}
			dest.Gauge(metricName, v)
		} else {
			v, err := strconv.ParseUint(metricValue, 10, 64)
			if err != nil {
				return errors.Wrap(err, "invalid gauge value")
			}
			dest.Gauge(metricName, v)
		}
	case "h": // histogram (circonus)
		fallthrough
	case "ms": // measurement
		v, err := strconv.ParseFloat(metricValue, 64)
		if err != nil {
			return errors.Wrap(err, "invalid histogram value")
		}
		if sampleRate > 0 {
			v /= sampleRate
		}
		dest.RecordValue(metricName, v)
	case "s": // set
		// in the case of sets, the value is the unique "thing" to be tracked
		// counters are used to track individual "things"
		dest.Increment(strings.Join([]string{metricName, metricValue}, "`"))
	case "t": // text (circonus)
		dest.SetText(metricName, metricValue)
	default:
		return errors.Errorf("invalid metric type (%s)", metricType)
	}

	s.logger.Debug().
		Str("metric", metric).
		Str("Name", metricName).
		Str("Type", metricType).
		Str("Value", metricValue).
		Str("Destination", metricDest).
		Msg("parsing")

	return nil
}

func (s *Server) initHostMetrics() error {
	s.hostMetricsmu.Lock()
	defer s.hostMetricsmu.Unlock()

	cmc := &cgm.Config{}
	cmc.Debug = viper.GetBool(config.KeyDebugCGM)
	cmc.Log = stdlog.New(s.logger.With().Str("pkg", "statsd-host-check").Logger(), "", 0)
	// put cgm into manual mode (no interval, no api key, invalid submission url)
	cmc.Interval = "0"                            // disable automatic flush
	cmc.CheckManager.Check.SubmissionURL = "none" // disable check management (create/update)

	hm, err := cgm.NewCirconusMetrics(cmc)
	if err != nil {
		return errors.Wrap(err, "statsd host check")
	}

	s.hostMetrics = hm

	s.logger.Info().Msg("statsd host check initialized")
	return nil
}

func (s *Server) initGroupMetrics() error {
	cid := viper.GetString(config.KeyStatsdGroupCID)
	if cid == "" {
		log.Info().Msg("statsd group check disabled")
		return nil
	}

	s.groupMetricsmu.Lock()
	defer s.groupMetricsmu.Unlock()

	cmc := &cgm.Config{}
	cmc.CheckManager.Check.ID = cid
	cmc.Debug = viper.GetBool(config.KeyDebugCGM)
	cmc.Log = stdlog.New(s.logger.With().Str("pkg", "statsd-group-check").Logger(), "", 0)

	gm, err := cgm.NewCirconusMetrics(cmc)
	if err != nil {
		return errors.Wrap(err, "statsd group check")
	}

	s.groupMetrics = gm

	s.logger.Info().Msg("statsd group check initialized")
	return nil
}
