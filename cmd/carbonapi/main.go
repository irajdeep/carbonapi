package main

import (
	"bytes"
	"encoding/json"
	"expvar"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/bookingcom/carbonapi/cache"
	"github.com/bookingcom/carbonapi/carbonapipb"
	"github.com/bookingcom/carbonapi/cfg"
	"github.com/bookingcom/carbonapi/expr/functions"
	"github.com/bookingcom/carbonapi/expr/functions/cairo/png"
	"github.com/bookingcom/carbonapi/expr/rewrite"
	"github.com/bookingcom/carbonapi/limiter"
	"github.com/bookingcom/carbonapi/mstats"
	"github.com/bookingcom/carbonapi/pathcache"
	"github.com/bookingcom/carbonapi/pkg/parser"
	"github.com/bookingcom/carbonapi/util"
	realZipper "github.com/bookingcom/carbonapi/zipper"
	pb "github.com/go-graphite/protocol/carbonapi_v2_pb"

	"io/ioutil"

	"github.com/facebookgo/grace/gracehttp"
	"github.com/facebookgo/pidfile"
	"github.com/gorilla/handlers"
	"github.com/lomik/zapwriter"
	"github.com/peterbourgon/g2g"
	"github.com/prometheus/client_golang/prometheus"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
)

var prometheusMetrics = struct {
	Requests     prometheus.Counter
	Responses    *prometheus.CounterVec
	DurationsExp prometheus.Histogram
	DurationsLin prometheus.Histogram
}{
	Requests: prometheus.NewCounter(
		prometheus.CounterOpts{
			Name: "http_requests_total",
			Help: "Count of HTTP requests",
		},
	),
	Responses: prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "http_responses_total",
			Help: "Count of HTTP responses, partitioned by return code and handler",
		},
		[]string{"code", "handler"},
	),
	DurationsExp: prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds_exp",
			Help:    "The duration of HTTP requests (exponential)",
			Buckets: prometheus.ExponentialBuckets((50 * time.Millisecond).Seconds(), 2.0, 20),
		},
	),
	DurationsLin: prometheus.NewHistogram(
		prometheus.HistogramOpts{
			Name:    "http_request_duration_seconds_lin",
			Help:    "The duration of HTTP requests (linear)",
			Buckets: prometheus.LinearBuckets(0.0, (50 * time.Millisecond).Seconds(), 40), // Up to 2 seconds
		},
	),
}

var apiMetrics = struct {
	// Total counts across all request types
	Requests  *expvar.Int
	Responses *expvar.Int
	Errors    *expvar.Int

	Goroutines    expvar.Func
	Uptime        expvar.Func
	LimiterUse    expvar.Func
	LimiterUseMax expvar.Func

	// Despite the names, these only count /render requests
	RenderRequests        *expvar.Int
	RequestCacheHits      *expvar.Int
	RequestCacheMisses    *expvar.Int
	RenderCacheOverheadNS *expvar.Int

	FindRequests        *expvar.Int
	FindCacheHits       *expvar.Int
	FindCacheMisses     *expvar.Int
	FindCacheOverheadNS *expvar.Int

	MemcacheTimeouts expvar.Func

	CacheSize  expvar.Func
	CacheItems expvar.Func
}{
	Requests:  expvar.NewInt("requests"),
	Responses: expvar.NewInt("responses"),
	Errors:    expvar.NewInt("errors"),

	// TODO: request_cache -> render_cache
	RenderRequests:        expvar.NewInt("render_requests"),
	RequestCacheHits:      expvar.NewInt("request_cache_hits"),
	RequestCacheMisses:    expvar.NewInt("request_cache_misses"),
	RenderCacheOverheadNS: expvar.NewInt("render_cache_overhead_ns"),

	FindRequests: expvar.NewInt("find_requests"),

	FindCacheHits:       expvar.NewInt("find_cache_hits"),
	FindCacheMisses:     expvar.NewInt("find_cache_misses"),
	FindCacheOverheadNS: expvar.NewInt("find_cache_overhead_ns"),
}

var zipperMetrics = struct {
	FindRequests *expvar.Int
	FindErrors   *expvar.Int

	RenderRequests *expvar.Int
	RenderErrors   *expvar.Int

	InfoRequests *expvar.Int
	InfoErrors   *expvar.Int

	Timeouts *expvar.Int

	CacheSize  expvar.Func
	CacheItems expvar.Func

	CacheMisses *expvar.Int
	CacheHits   *expvar.Int
}{
	FindRequests: expvar.NewInt("zipper_find_requests"),
	FindErrors:   expvar.NewInt("zipper_find_errors"),

	RenderRequests: expvar.NewInt("zipper_render_requests"),
	RenderErrors:   expvar.NewInt("zipper_render_errors"),

	InfoRequests: expvar.NewInt("zipper_info_requests"),
	InfoErrors:   expvar.NewInt("zipper_info_errors"),

	Timeouts: expvar.NewInt("zipper_timeouts"),

	CacheHits:   expvar.NewInt("zipper_cache_hits"),
	CacheMisses: expvar.NewInt("zipper_cache_misses"),
}

const (
	localHostName = ""
)

// BuildVersion is provided to be overridden at build time. Eg. go build -ldflags -X 'main.BuildVersion=...'
var BuildVersion = "(development build)"

// for testing
var timeNow = time.Now

func buildParseErrorString(target, e string, err error) string {
	msg := fmt.Sprintf("%s\n\n%-20s: %s\n", http.StatusText(http.StatusBadRequest), "Target", target)
	if err != nil {
		msg += fmt.Sprintf("%-20s: %s\n", "Error", err.Error())
	}
	if e != "" {
		msg += fmt.Sprintf("%-20s: %s\n%-20s: %s\n",
			"Parsed so far", target[0:len(target)-len(e)],
			"Could not parse", e)
	}
	return msg
}

func deferredAccessLogging(r *http.Request, accessLogDetails *carbonapipb.AccessLogDetails, t time.Time, logAsError bool) {
	accessLogger := zapwriter.Logger("access")

	accessLogDetails.Runtime = time.Since(t).Seconds()
	accessLogDetails.RequestMethod = r.Method
	if logAsError {
		accessLogger.Error("request failed", zap.Any("data", *accessLogDetails))
		apiMetrics.Errors.Add(1)
	} else {
		accessLogDetails.HttpCode = http.StatusOK
		accessLogger.Info("request served", zap.Any("data", *accessLogDetails))
		apiMetrics.Responses.Add(1)
	}
	prometheusMetrics.Responses.WithLabelValues(fmt.Sprintf("%d", accessLogDetails.HttpCode), accessLogDetails.Handler).Inc()
}

type treejson struct {
	AllowChildren int            `json:"allowChildren"`
	Expandable    int            `json:"expandable"`
	Leaf          int            `json:"leaf"`
	ID            string         `json:"id"`
	Text          string         `json:"text"`
	Context       map[string]int `json:"context"` // unused
}

var treejsonContext = make(map[string]int)

func findTreejson(globs pb.GlobResponse) ([]byte, error) {
	var b bytes.Buffer

	var tree = make([]treejson, 0)

	seen := make(map[string]struct{})

	basepath := globs.Name

	if i := strings.LastIndex(basepath, "."); i != -1 {
		basepath = basepath[:i+1]
	} else {
		basepath = ""
	}

	for _, g := range globs.Matches {

		name := g.Path

		if i := strings.LastIndex(name, "."); i != -1 {
			name = name[i+1:]
		}

		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}

		t := treejson{
			ID:      basepath + name,
			Context: treejsonContext,
			Text:    name,
		}

		if g.IsLeaf {
			t.Leaf = 1
		} else {
			t.AllowChildren = 1
			t.Expandable = 1
		}

		tree = append(tree, t)
	}

	err := json.NewEncoder(&b).Encode(tree)
	return b.Bytes(), err
}

var config = struct {
	cfg.API

	queryCache       cache.BytesCache
	findCache        cache.BytesCache
	blockHeaderRules RuleConfig

	defaultTimeZone *time.Location

	zipper CarbonZipper

	// Limiter limits concurrent zipper requests
	limiter limiter.ServerLimiter
}{
	API: cfg.DefaultAPIConfig,

	queryCache: cache.NullCache{},
	findCache:  cache.NullCache{},

	defaultTimeZone: time.Local,
}

func zipperStats(stats *realZipper.Stats) {
	zipperMetrics.Timeouts.Add(stats.Timeouts)

	zipperMetrics.FindErrors.Add(stats.FindErrors)
	zipperMetrics.RenderErrors.Add(stats.RenderErrors)
	zipperMetrics.InfoErrors.Add(stats.InfoErrors)

	zipperMetrics.CacheMisses.Add(stats.CacheMisses)
	zipperMetrics.CacheHits.Add(stats.CacheHits)
}

var graphTemplates map[string]png.PictureParams

func setUpConfig(logger *zap.Logger, zipper CarbonZipper) {
	err := zapwriter.ApplyConfig(config.Logger)
	if err != nil {
		logger.Fatal("failed to initialize logger with requested configuration",
			zap.Any("configuration", config.Logger),
			zap.Error(err),
		)
	}

	for name, color := range config.DefaultColors {
		if err := png.SetColor(name, color); err != nil {
			logger.Warn("invalid color specified and will be ignored",
				zap.String("reason", "color must be valid hex rgb or rbga value, e.x. '#c80032', 'c80032', 'c80032ff', etc."),
				zap.Error(err),
			)
		}
	}

	rewrite.New(config.FunctionsConfigs)
	functions.New(config.FunctionsConfigs)

	expvar.NewString("GoVersion").Set(runtime.Version())
	expvar.NewString("BuildVersion").Set(BuildVersion)
	expvar.Publish("config", expvar.Func(func() interface{} { return config }))

	apiMetrics.Goroutines = expvar.Func(func() interface{} {
		return runtime.NumGoroutine()
	})
	expvar.Publish("goroutines", apiMetrics.Goroutines)

	startMinute := time.Now().Unix() / 60
	apiMetrics.Uptime = expvar.Func(func() interface{} {
		return time.Now().Unix()/60 - startMinute
	})
	expvar.Publish("uptime", apiMetrics.Uptime)

	// TODO(gmagnusson): Shouldn't limiter live in config.zipper?
	config.limiter = limiter.NewServerLimiter([]string{localHostName}, config.ConcurrencyLimitPerServer)
	config.zipper = zipper

	apiMetrics.LimiterUse = expvar.Func(func() interface{} {
		return config.limiter.LimiterUse()[localHostName]
	})
	expvar.Publish("limiter_use", apiMetrics.LimiterUse)

	apiMetrics.LimiterUseMax = expvar.Func(func() interface{} {
		return config.limiter.MaxLimiterUse()
	})
	expvar.Publish("limiter_use_max", apiMetrics.LimiterUseMax)

	switch config.Cache.Type {
	case "memcache":
		if len(config.Cache.MemcachedServers) == 0 {
			logger.Fatal("memcache cache requested but no memcache servers provided")
		}

		logger.Info("memcached configured",
			zap.Strings("servers", config.Cache.MemcachedServers),
		)
		config.queryCache = cache.NewMemcached("capi", config.Cache.MemcachedServers...)
		// find cache is only used if SendGlobsAsIs is false.
		if !config.SendGlobsAsIs {
			config.findCache = cache.NewExpireCache(0)
		}

		mcache := config.queryCache.(*cache.MemcachedCache)

		apiMetrics.MemcacheTimeouts = expvar.Func(func() interface{} {
			return mcache.Timeouts()
		})
		expvar.Publish("memcache_timeouts", apiMetrics.MemcacheTimeouts)

	case "mem":
		config.queryCache = cache.NewExpireCache(uint64(config.Cache.Size * 1024 * 1024))

		// find cache is only used if SendGlobsAsIs is false.
		if !config.SendGlobsAsIs {
			config.findCache = cache.NewExpireCache(0)
		}

		qcache := config.queryCache.(*cache.ExpireCache)

		apiMetrics.CacheSize = expvar.Func(func() interface{} {
			return qcache.Size()
		})
		expvar.Publish("cache_size", apiMetrics.CacheSize)

		apiMetrics.CacheItems = expvar.Func(func() interface{} {
			return qcache.Items()
		})
		expvar.Publish("cache_items", apiMetrics.CacheItems)

	case "null":
		// defaults
		config.queryCache = cache.NullCache{}
		config.findCache = cache.NullCache{}
	default:
		logger.Error("unknown cache type",
			zap.String("cache_type", config.Cache.Type),
			zap.Strings("known_cache_types", []string{"null", "mem", "memcache"}),
		)
	}

	if config.TimezoneString != "" {
		fields := strings.Split(config.TimezoneString, ",")

		if len(fields) != 2 {
			logger.Fatal("unexpected amount of fields in tz",
				zap.String("timezone_string", config.TimezoneString),
				zap.Int("fields_got", len(fields)),
				zap.Int("fields_expected", 2),
			)
		}

		offs, err := strconv.Atoi(fields[1])
		if err != nil {
			logger.Fatal("unable to parse seconds",
				zap.String("field[1]", fields[1]),
				zap.Error(err),
			)
		}

		config.defaultTimeZone = time.FixedZone(fields[0], offs)
		logger.Info("using fixed timezone",
			zap.String("timezone", config.defaultTimeZone.String()),
			zap.Int("offset", offs),
		)
	}

	if len(config.UnicodeRangeTables) != 0 {
		for _, stringRange := range config.UnicodeRangeTables {
			parser.RangeTables = append(parser.RangeTables, unicode.Scripts[stringRange])
		}
	} else {
		parser.RangeTables = append(parser.RangeTables, unicode.Latin)
	}

	if config.MaxProcs != 0 {
		runtime.GOMAXPROCS(config.MaxProcs)
	}

	var host string
	if envhost := os.Getenv("GRAPHITEHOST") + ":" + os.Getenv("GRAPHITEPORT"); envhost != ":" || config.Graphite.Host != "" {
		switch {
		case envhost != ":" && config.Graphite.Host != "":
			host = config.Graphite.Host
		case envhost != ":":
			host = envhost
		case config.Graphite.Host != "":
			host = config.Graphite.Host
		}
	}

	logger.Info("starting carbonapi",
		zap.String("build_version", BuildVersion),
		zap.Any("config", config),
	)

	// +1 to track every over the number of buckets we track
	timeBuckets = make([]int64, config.Buckets+1)
	expTimeBuckets = make([]int64, config.Buckets+1)
	expvar.Publish("requestBuckets", expvar.Func(renderTimeBuckets))
	expvar.Publish("expRequestBuckets", expvar.Func(renderExpTimeBuckets))

	if host != "" {
		// register our metrics with graphite
		graphite := g2g.NewGraphite(host, config.Graphite.Interval, 10*time.Second)

		hostname, _ := os.Hostname()
		hostname = strings.Replace(hostname, ".", "_", -1)

		prefix := config.Graphite.Prefix

		pattern := config.Graphite.Pattern
		pattern = strings.Replace(pattern, "{prefix}", prefix, -1)
		pattern = strings.Replace(pattern, "{fqdn}", hostname, -1)

		graphite.Register(fmt.Sprintf("%s.requests", pattern), apiMetrics.Requests)
		graphite.Register(fmt.Sprintf("%s.responses", pattern), apiMetrics.Responses)
		graphite.Register(fmt.Sprintf("%s.errors", pattern), apiMetrics.Errors)

		for i := 0; i <= config.Buckets; i++ {
			graphite.Register(fmt.Sprintf("%s.requests_in_%dms_to_%dms", pattern, i*100, (i+1)*100), bucketEntry(i))
			lower, upper := util.Bounds(i)
			graphite.Register(fmt.Sprintf("%s.exp.requests_in_%05dms_to_%05dms", pattern, lower, upper), bucketEntry(i))
		}

		graphite.Register(fmt.Sprintf("%s.request_cache_hits", pattern), apiMetrics.RequestCacheHits)
		graphite.Register(fmt.Sprintf("%s.request_cache_misses", pattern), apiMetrics.RequestCacheMisses)
		graphite.Register(fmt.Sprintf("%s.request_cache_overhead_ns", pattern), apiMetrics.RenderCacheOverheadNS)

		graphite.Register(fmt.Sprintf("%s.find_requests", pattern), apiMetrics.FindRequests)
		graphite.Register(fmt.Sprintf("%s.find_cache_hits", pattern), apiMetrics.FindCacheHits)
		graphite.Register(fmt.Sprintf("%s.find_cache_misses", pattern), apiMetrics.FindCacheMisses)
		graphite.Register(fmt.Sprintf("%s.find_cache_overhead_ns", pattern), apiMetrics.FindCacheOverheadNS)

		graphite.Register(fmt.Sprintf("%s.render_requests", pattern), apiMetrics.RenderRequests)

		if apiMetrics.MemcacheTimeouts != nil {
			graphite.Register(fmt.Sprintf("%s.memcache_timeouts", pattern), apiMetrics.MemcacheTimeouts)
		}

		if apiMetrics.CacheSize != nil {
			graphite.Register(fmt.Sprintf("%s.cache_size", pattern), apiMetrics.CacheSize)
			graphite.Register(fmt.Sprintf("%s.cache_items", pattern), apiMetrics.CacheItems)
		}

		graphite.Register(fmt.Sprintf("%s.zipper.find_requests", pattern), zipperMetrics.FindRequests)
		graphite.Register(fmt.Sprintf("%s.zipper.find_errors", pattern), zipperMetrics.FindErrors)

		graphite.Register(fmt.Sprintf("%s.zipper.render_requests", pattern), zipperMetrics.RenderRequests)
		graphite.Register(fmt.Sprintf("%s.zipper.render_errors", pattern), zipperMetrics.RenderErrors)

		graphite.Register(fmt.Sprintf("%s.zipper.info_requests", pattern), zipperMetrics.InfoRequests)
		graphite.Register(fmt.Sprintf("%s.zipper.info_errors", pattern), zipperMetrics.InfoErrors)

		graphite.Register(fmt.Sprintf("%s.zipper.timeouts", pattern), zipperMetrics.Timeouts)

		graphite.Register(fmt.Sprintf("%s.zipper.cache_size", pattern), zipperMetrics.CacheSize)
		graphite.Register(fmt.Sprintf("%s.zipper.cache_items", pattern), zipperMetrics.CacheItems)

		graphite.Register(fmt.Sprintf("%s.zipper.cache_hits", pattern), zipperMetrics.CacheHits)
		graphite.Register(fmt.Sprintf("%s.zipper.cache_misses", pattern), zipperMetrics.CacheMisses)

		go mstats.Start(config.Graphite.Interval)

		graphite.Register(fmt.Sprintf("%s.goroutines", pattern), apiMetrics.Goroutines)
		graphite.Register(fmt.Sprintf("%s.uptime", pattern), apiMetrics.Uptime)
		graphite.Register(fmt.Sprintf("%s.max_limiter_use", pattern), apiMetrics.LimiterUseMax)
		graphite.Register(fmt.Sprintf("%s.limiter_use", pattern), apiMetrics.LimiterUse)
		graphite.Register(fmt.Sprintf("%s.alloc", pattern), &mstats.Alloc)
		graphite.Register(fmt.Sprintf("%s.total_alloc", pattern), &mstats.TotalAlloc)
		graphite.Register(fmt.Sprintf("%s.num_gc", pattern), &mstats.NumGC)
		graphite.Register(fmt.Sprintf("%s.pause_ns", pattern), &mstats.PauseNS)

	}

	if config.PidFile != "" {
		pidfile.SetPidfilePath(config.PidFile)
		err := pidfile.Write()
		if err != nil {
			logger.Fatal("error during pidfile.Write()",
				zap.Error(err),
			)
		}
	}
}

func setUpConfigUpstreams(logger *zap.Logger) {
	if len(config.Backends) == 0 {
		logger.Fatal("no backends specified for upstreams!")
	}

	// Setup in-memory path cache for carbonzipper requests
	config.PathCache = pathcache.NewPathCache(config.ExpireDelaySec)

	zipperMetrics.CacheSize = expvar.Func(func() interface{} { return config.PathCache.ECSize() })
	expvar.Publish("cacheSize", zipperMetrics.CacheSize)

	zipperMetrics.CacheItems = expvar.Func(func() interface{} { return config.PathCache.ECItems() })
	expvar.Publish("cacheItems", zipperMetrics.CacheItems)
}

var timeBuckets []int64
var expTimeBuckets []int64

type bucketEntry int
type expBucketEntry int

func (b bucketEntry) String() string {
	return strconv.Itoa(int(atomic.LoadInt64(&timeBuckets[b])))
}

func (b expBucketEntry) String() string {
	return strconv.Itoa(int(atomic.LoadInt64(&expTimeBuckets[b])))
}

func renderTimeBuckets() interface{} {
	return timeBuckets
}

func renderExpTimeBuckets() interface{} {
	return timeBuckets
}

func findBucketIndex(buckets []int64, bucket int) int {
	var i int
	if bucket < 0 {
		i = 0
	} else if bucket < len(buckets)-1 {
		i = bucket
	} else {
		i = len(buckets) - 1
	}

	return i
}

func bucketRequestTimes(req *http.Request, t time.Duration) {
	logger := zapwriter.Logger("slow")

	ms := t.Nanoseconds() / int64(time.Millisecond)

	bucket := int(ms / 100)
	bucketIdx := findBucketIndex(timeBuckets, bucket)
	atomic.AddInt64(&timeBuckets[bucketIdx], 1)

	expBucket := util.Bucket(ms, config.Buckets)
	expBucketIdx := findBucketIndex(expTimeBuckets, expBucket)
	atomic.AddInt64(&expTimeBuckets[expBucketIdx], 1)

	prometheusMetrics.DurationsExp.Observe(t.Seconds())
	prometheusMetrics.DurationsLin.Observe(t.Seconds())

	// This seems slow enough to count as a slow request
	if bucket >= config.Buckets {
		logger.Warn("Slow Request",
			zap.Duration("time", t),
			zap.String("url", req.URL.String()),
		)
	}
}

func main() {
	err := zapwriter.ApplyConfig([]zapwriter.Config{cfg.DefaultLoggerConfig})
	if err != nil {
		log.Fatal("Failed to initialize logger with default configuration")
	}
	logger := zapwriter.Logger("main")

	configPath := flag.String("config", "", "Path to the `config file`.")
	flag.Parse()

	fh, err := os.Open(*configPath)
	if err != nil {
		logger.Fatal("Failed to open config file",
			zap.Error(err),
		)
	}

	api, err := cfg.ParseAPIConfig(fh)
	if err != nil {
		logger.Fatal("Failed to parse config file",
			zap.Error(err),
		)
	}
	config.API = api
	fh.Close()

	setUpConfigUpstreams(logger)
	zipper := newZipper(zipperStats, config.Zipper, logger.With(zap.String("handler", "zipper")))
	setUpConfig(logger, zipper)

	handler := initHandlers()
	handler = handlers.CompressHandler(handler)
	handler = handlers.CORS()(handler)
	handler = handlers.ProxyHeaders(handler)
	handler = util.UUIDHandler(handler)

	go func() {
		prometheus.MustRegister(prometheusMetrics.Requests)
		prometheus.MustRegister(prometheusMetrics.Responses)
		prometheus.MustRegister(prometheusMetrics.DurationsExp)
		prometheus.MustRegister(prometheusMetrics.DurationsLin)

		writeTimeout := config.Timeouts.Global
		if writeTimeout < 30*time.Second {
			writeTimeout = time.Minute
		}

		s := &http.Server{
			Addr:         config.ListenInternal,
			Handler:      initHandlersInternal(),
			ReadTimeout:  1 * time.Second,
			WriteTimeout: writeTimeout,
		}

		if err := s.ListenAndServe(); err != nil {
			logger.Fatal("Internal handle server failed",
				zap.Error(err),
			)
		}
	}()

	if config.BlockHeaderUpdatePeriod > 0 {
		ticker := time.NewTicker(config.BlockHeaderUpdatePeriod)
		go loadBlockRuleHeaderConfig(ticker, logger)
	}

	err = gracehttp.Serve(&http.Server{
		Addr:         config.Listen,
		Handler:      handler,
		ReadTimeout:  1 * time.Second,
		WriteTimeout: config.Timeouts.Global,
	})

	if err != nil {
		logger.Fatal("gracehttp failed",
			zap.Error(err),
		)
	}
}

func loadBlockRuleHeaderConfig(ticker *time.Ticker, logger *zap.Logger) {
	var ruleConfig RuleConfig
	for range ticker.C {
		fileData, err := loadBlockRuleConfig()

		if err == nil {
			err = yaml.Unmarshal(fileData, &ruleConfig)
			if err != nil {
				logger.Error("couldn't unmarshal block rule file data")
			} else {
				config.blockHeaderRules = ruleConfig
			}
		} else {
			config.blockHeaderRules = RuleConfig{}
		}
	}
}

func loadBlockRuleConfig() ([]byte, error) {
	fileLock.Lock()
	defer fileLock.Unlock()
	fileData, err := ioutil.ReadFile(config.BlockHeaderFile)
	return fileData, err
}
