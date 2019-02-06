package metrics

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/mailgun/gubernator/cache"
	"github.com/mailgun/holster"
	"github.com/sirupsen/logrus"
	"github.com/smira/go-statsd"
	"google.golang.org/grpc/stats"
)

type StatsdClient interface {
	Gauge(string, int64)
	Inc(string, int64)
	Close() error
}

type NullClient struct{}

func (n *NullClient) Gauge(string, int64) {}
func (n *NullClient) Inc(string, int64)   {}
func (n *NullClient) Close() error        { return nil }

type StatsdMetrics struct {
	reqChan    chan *RequestStats
	cacheStats cache.CacheStats
	wg         holster.WaitGroup
	client     StatsdClient
	log        *logrus.Entry
}

func NewStatsdMetrics(client StatsdClient) *StatsdMetrics {
	sd := StatsdMetrics{
		client: client,
		log:    logrus.WithField("category", "metrics"),
	}
	return &sd
}

func (sd *StatsdMetrics) Start() error {
	sd.reqChan = make(chan *RequestStats, 10000)
	methods := make(map[string]RequestStats)

	tick := time.NewTicker(time.Second)
	sd.wg.Until(func(done chan struct{}) bool {
		select {
		case stat := <-sd.reqChan:
			// Aggregate GRPC method stats
			item, ok := methods[stat.Method]
			if ok {
				item.Failed += stat.Failed
				item.Called += 1
				if item.Duration > stat.Duration {
					item.Duration = stat.Duration
				}
				return true
			}
			stat.Called = 1
			methods[stat.Method] = *stat
		case <-tick.C:
			// Emit stats about GRPC method calls
			for k, v := range methods {
				method := k[strings.LastIndex(k, "/")+1:]
				sd.client.Gauge(fmt.Sprintf("api.%s.duration", method), int64(v.Duration))
				sd.client.Inc(fmt.Sprintf("api.%s.total", method), v.Called)
				sd.client.Inc(fmt.Sprintf("api.%s.failed", method), v.Failed)
			}
			// Clear the current method stats
			methods = make(map[string]RequestStats, len(methods))

			// Emit stats about our cache
			if sd.cacheStats != nil {
				stats := sd.cacheStats.Stats(true)
				sd.client.Inc("cache.size", stats.Size)
				sd.client.Inc("cache.hit", stats.Hit)
				sd.client.Inc("cache.miss", stats.Miss)
			}
		case <-done:
			tick.Stop()
			sd.client.Close()
			return false
		}
		return true
	})
	return nil
}

func (sd *StatsdMetrics) Stop() {
	sd.wg.Stop()
}

func (sd *StatsdMetrics) HandleRPC(ctx context.Context, s stats.RPCStats) {
	rs := StatsFromContext(ctx)
	if rs == nil {
		return
	}

	switch t := s.(type) {
	// case *stats.Begin:
	// case *stats.InPayload:
	// case *stats.InHeader:
	// case *stats.InTrailer:
	// case *stats.OutPayload:
	// case *stats.OutHeader:
	// case *stats.OutTrailer:
	case *stats.End:
		rs.Duration = t.EndTime.Sub(t.BeginTime)
		if t.Error != nil {
			rs.Failed = 1
		}
		sd.reqChan <- rs
	}
}

func (sd *StatsdMetrics) GRPCStatsHandler() stats.Handler                   { return sd }
func (sd *StatsdMetrics) HandleConn(ctx context.Context, s stats.ConnStats) {}
func (sd *StatsdMetrics) RegisterCacheStats(c cache.CacheStats)             { sd.cacheStats = c }

func (sd *StatsdMetrics) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (sd *StatsdMetrics) TagRPC(ctx context.Context, tagInfo *stats.RPCTagInfo) context.Context {
	return ContextWithStats(ctx, &RequestStats{Method: tagInfo.FullMethodName})
}

// Adapt a statsd client to our interface
type Adaptor struct {
	Client *statsd.Client
}

func (n *Adaptor) Gauge(stat string, count int64) {
	n.Client.Gauge(stat, count)
}

func (n *Adaptor) Inc(stat string, count int64) {
	n.Client.Incr(stat, count)
}

func (n *Adaptor) Close() error {
	return n.Client.Close()
}

type StatsdConfig struct {
	Interval time.Duration
	Endpoint string
	Prefix   string
}
