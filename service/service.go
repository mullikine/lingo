package service

import (
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/codelingo/lingo/app/util/common/config"
	"github.com/codelingo/lingo/vcs"
	"github.com/codelingo/lingo/vcs/backing"
	"github.com/juju/errors"

	"github.com/lightstep/lightstep-tracer-go"
	"github.com/opentracing/opentracing-go"
	zipkin "github.com/openzipkin/zipkin-go-opentracing"
	appdashot "github.com/sourcegraph/appdash/opentracing"
	"golang.org/x/net/context"
	"sourcegraph.com/sourcegraph/appdash"

	grpcclient "github.com/codelingo/lingo/service/grpc"
	"github.com/codelingo/lingo/service/grpc/codelingo"
	"github.com/go-kit/kit/endpoint"
	"github.com/go-kit/kit/loadbalancer"
	"github.com/go-kit/kit/loadbalancer/static"
	"github.com/go-kit/kit/log"
	// kitot "github.com/go-kit/kit/tracing/opentracing"

	"github.com/codelingo/lingo/service/server"
)

func grpcAddress() (string, error) {
	cfg, err := config.Platform()
	if err != nil {
		return "", errors.Trace(err)
	}

	return cfg.Addr + ":" + cfg.GrpcPort, nil
}

type client struct {
	context.Context
	log.Logger
	endpoints map[string]endpoint.Endpoint
}

// TODO(pb): If your service interface methods don't return an error, we have
// no way to signal problems with a service client. If they don't take a
// context, we have to provide a global context for any transport that
// requires one, effectively making your service a black box to any context-
// specific information. So, we should make some recommendations:
//
// - To get started, a simple service interface is probably fine.
//
// - To properly deal with transport errors, every method on your service
//   should return an error. This is probably important.
//
// - To properly deal with context information, every method on your service
//   can take a context as its first argument. This may or may not be
//   important.

func (c client) Query(clql string) (string, error) {
	request := server.QueryRequest{
		CLQL: clql,
	}
	reply, err := c.endpoints["query"](c.Context, request)
	if err != nil {
		c.Logger.Log("err", err)
		return "", err
	}

	r := reply.(server.QueryResponse)
	return r.Result, nil
}

func (c client) Review(req *server.ReviewRequest) ([]*codelingo.Issue, error) {
	// TODO(waigani) pass this in as opt
	repo := vcs.New(backing.Git)

	// set defaults
	if req.Owner == "" {
		return nil, errors.New("repository owner is not set")
	}
	if req.Repo == "" {
		return nil, errors.New("repository name is not set")
	}

	if req.SHA == "" {

		sha, err := repo.CurrentCommitId()
		if err != nil {
			return nil, errors.Trace(err)
		}
		req.SHA = sha
	}

	if err := repo.Sync(); err != nil {
		return nil, err
	}

	var err error
	req.Patches, err = repo.Patches()
	if err != nil {
		return nil, errors.Trace(err)
	}

	reply, err := c.endpoints["review"](c.Context, req)
	if err != nil {
		c.Logger.Log("err", err)
		return nil, err
	}

	return reply.([]*codelingo.Issue), nil
}

// TODO(waigani) construct logger separately and pass into New.
// TODO(waigani) swap os.Exit(1) for return err

// NewClient returns a QueryService that's backed by the provided Endpoints
func New() (server.CodeLingoService, error) {
	grpcAddr, err := grpcAddress()
	if err != nil {
		return nil, errors.Trace(err)
	}

	var (
		grpcAddrs = flag.String("grpc.addrs", grpcAddr, "Comma-separated list of addresses for gRPC servers")

		// Three OpenTracing backends (to demonstrate how they can be interchanged):
		zipkinAddr           = flag.String("zipkin.kafka.addr", "", "Enable Zipkin tracing via a Kafka Collector host:port")
		appdashAddr          = flag.String("appdash.addr", "", "Enable Appdash tracing via an Appdash server host:port")
		lightstepAccessToken = flag.String("lightstep.token", "", "Enable LightStep tracing via a LightStep access token")
	)
	flag.Parse()
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "\n%s [flags] method\n\n", filepath.Base(os.Args[0]))
		flag.Usage()
		os.Exit(1)
	}

	randomSeed := time.Now().UnixNano()

	var logger log.Logger
	logger = log.NewLogfmtLogger(os.Stdout)
	logger = log.NewContext(logger).With("caller", log.DefaultCaller)
	logger = log.NewContext(logger).With("transport", "grpc")
	tracingLogger := log.NewContext(logger).With("component", "tracing")

	// Set up OpenTracing
	var tracer opentracing.Tracer
	{
		switch {
		case *appdashAddr != "" && *lightstepAccessToken == "" && *zipkinAddr == "":
			tracer = appdashot.NewTracer(appdash.NewRemoteCollector(*appdashAddr))
		case *appdashAddr == "" && *lightstepAccessToken != "" && *zipkinAddr == "":
			tracer = lightstep.NewTracer(lightstep.Options{
				AccessToken: *lightstepAccessToken,
			})
			defer lightstep.FlushLightStepTracer(tracer)
		case *appdashAddr == "" && *lightstepAccessToken == "" && *zipkinAddr != "":
			collector, err := zipkin.NewKafkaCollector(
				strings.Split(*zipkinAddr, ","),
				zipkin.KafkaLogger(tracingLogger),
			)
			if err != nil {
				tracingLogger.Log("err", "unable to create kafka collector", "fatal", err)
				os.Exit(1)
			}
			tracer, err = zipkin.NewTracer(
				zipkin.NewRecorder(collector, false, "localhost:8000", "querysvc-client"),
			)
			if err != nil {
				tracingLogger.Log("err", "unable to create zipkin tracer", "fatal", err)
				os.Exit(1)
			}
		case *appdashAddr == "" && *lightstepAccessToken == "" && *zipkinAddr == "":
			tracer = opentracing.GlobalTracer() // no-op
		default:
			tracingLogger.Log("fatal", "specify a single -appdash.addr, -lightstep.access.token or -zipkin.kafka.addr")
			os.Exit(1)
		}
	}

	instances := strings.Split(*grpcAddrs, ",")
	queryFactory := grpcclient.MakeQueryEndpointFactory(tracer, tracingLogger)
	reviewFactory := grpcclient.MakeReviewEndpointFactory(tracer, tracingLogger)

	return client{
		Context: context.Background(),
		Logger:  logger,
		endpoints: map[string]endpoint.Endpoint{
			// TODO(waigani) this could be refactored further, a lot of dups
			"query":  buildEndpoint(tracer, "query", instances, queryFactory, randomSeed, logger),
			"review": buildEndpoint(tracer, "review", instances, reviewFactory, randomSeed, logger),
		},
	}, nil
}

func buildEndpoint(tracer opentracing.Tracer, operationName string, instances []string, factory loadbalancer.Factory, seed int64, logger log.Logger) endpoint.Endpoint {
	publisher := static.NewPublisher(instances, factory, logger)
	random := loadbalancer.NewRandom(publisher, seed)
	endpoint := loadbalancer.Retry(10, 10*time.Second, random)
	return endpoint
	// return kitot.TraceClient(tracer, operationName)(endpoint)
}
