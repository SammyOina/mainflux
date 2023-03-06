package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"time"

	"github.com/cenkalti/backoff/v4"
	thingsClient "github.com/mainflux/mainflux/internal/clients/grpc/things"
	jaegerClient "github.com/mainflux/mainflux/internal/clients/jaeger"
	redisClient "github.com/mainflux/mainflux/internal/clients/redis"
	"github.com/mainflux/mainflux/internal/env"
	mflog "github.com/mainflux/mainflux/logger"
	"github.com/mainflux/mainflux/mqtt"
	mqttredis "github.com/mainflux/mainflux/mqtt/redis"
	"github.com/mainflux/mainflux/pkg/auth"
	"github.com/mainflux/mainflux/pkg/errors"
	"github.com/mainflux/mainflux/pkg/messaging"
	"github.com/mainflux/mainflux/pkg/messaging/brokers"
	mqttpub "github.com/mainflux/mainflux/pkg/messaging/mqtt"
	mp "github.com/mainflux/mproxy/pkg/mqtt"
	"github.com/mainflux/mproxy/pkg/session"
	ws "github.com/mainflux/mproxy/pkg/websocket"
	"golang.org/x/sync/errgroup"
)

const (
	svcName            = "mqtt"
	envPrefix          = "MF_MQTT_ADAPTER_"
	envPrefixES        = "MF_MQTT_ADAPTER_ES_"
	envPrefixAuthCache = "MF_AUTH_CACHE_"
)

type config struct {
	LogLevel              string        `env:"MF_MQTT_ADAPTER_LOG_LEVEL"                    envDefault:"info"`
	MqttPort              string        `env:"MF_MQTT_ADAPTER_MQTT_PORT"                    envDefault:"1883"`
	MqttTargetHost        string        `env:"MF_MQTT_ADAPTER_MQTT_TARGET_HOST"             envDefault:"localhost"`
	MqttTargetPort        string        `env:"MF_MQTT_ADAPTER_MQTT_TARGET_PORT"             envDefault:"1883"`
	MqttForwarderTimeout  time.Duration `env:"MF_MQTT_ADAPTER_FORWARDER_TIMEOUT"            envDefault:"30s"`
	MqttTargetHealthCheck string        `env:"MF_MQTT_ADAPTER_MQTT_TARGET_HEALTH_CHECK"     envDefault:""`
	HttpPort              string        `env:"MF_MQTT_ADAPTER_WS_PORT"                      envDefault:"8080"`
	HttpTargetHost        string        `env:"MF_MQTT_ADAPTER_WS_TARGET_HOST"               envDefault:"localhost"`
	HttpTargetPort        string        `env:"MF_MQTT_ADAPTER_WS_TARGET_PORT"               envDefault:"8080"`
	HttpTargetPath        string        `env:"MF_MQTT_ADAPTER_WS_TARGET_PATH"               envDefault:"/mqtt"`
	Instance              string        `env:"MF_MQTT_ADAPTER_INSTANCE"                     envDefault:""`
	JaegerURL             string        `env:"MF_JAEGER_URL"                                envDefault:"localhost:6831"`
	BrokerURL             string        `env:"MF_BROKER_URL"                                envDefault:"nats://localhost:4222"`
}

func main() {
	ctx, cancel := context.WithCancel(context.Background())
	g, ctx := errgroup.WithContext(ctx)

	cfg := config{}
	if err := env.Parse(&cfg); err != nil {
		log.Fatalf("failed to load %s configuration : %s", svcName, err)
	}

	logger, err := mflog.New(os.Stdout, cfg.LogLevel)
	if err != nil {
		log.Fatalf("failed to init logger: %s", err)
	}

	if cfg.MqttTargetHealthCheck != "" {
		notify := func(e error, next time.Duration) {
			logger.Info(fmt.Sprintf("Broker not ready: %s, next try in %s", e.Error(), next))
		}

		err := backoff.RetryNotify(healthcheck(cfg), backoff.NewExponentialBackOff(), notify)
		if err != nil {
			logger.Fatal(fmt.Sprintf("MQTT healthcheck limit exceeded, exiting. %s ", err))
		}
	}

	// PUB SUB tracer
	tracer, traceCloser, err := jaegerClient.NewTracer("nats_pubsub", cfg.JaegerURL)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to init Jaeger: %s", err))
	}
	defer traceCloser.Close()

	// mqtt tracer
	mqttTracer, mqttTraceCloser, err := jaegerClient.NewTracer("mqtt_adapter", cfg.JaegerURL)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to init Jaeger: %s", err))
	}
	defer mqttTraceCloser.Close()

	nps, err := brokers.NewPubSub(cfg.BrokerURL, "mqtt", logger, tracer)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to connect to message broker: %s", err))
	}
	defer nps.Close()

	mpub, err := mqttpub.NewPublisher(fmt.Sprintf("%s:%s", cfg.MqttTargetHost, cfg.MqttTargetPort), cfg.MqttForwarderTimeout)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to create MQTT publisher: %s", err))
	}

	fwd := mqtt.NewForwarder(brokers.SubjectAllChannels, logger, mqttTracer)
	if err := fwd.Forward(svcName, nps, mpub); err != nil {
		logger.Fatal(fmt.Sprintf("failed to forward message broker messages: %s", err))
	}

	np, err := brokers.NewPublisher(cfg.BrokerURL, tracer)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to connect to message broker: %s", err))
	}
	defer np.Close()

	ec, err := redisClient.Setup(envPrefixES)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to setup %s event store redis client : %s", svcName, err))
	}
	defer ec.Close()

	es := mqttredis.NewEventStore(ec, cfg.Instance)

	ac, err := redisClient.Setup(envPrefixAuthCache)
	if err != nil {
		logger.Fatal(fmt.Sprintf("failed to setup %s event store redis client : %s", svcName, err))
	}
	defer ac.Close()

	tc, tcHandler, err := thingsClient.Setup(envPrefix, cfg.JaegerURL)
	if err != nil {
		logger.Fatal(err.Error())
	}
	defer tcHandler.Close()
	logger.Info("Successfully connected to things grpc server " + tcHandler.Secure())

	authClient := auth.New(ac, tc)

	h := mqtt.NewHandler([]messaging.Publisher{np}, es, logger, authClient, mqttTracer)

	logger.Info(fmt.Sprintf("Starting MQTT proxy on port %s", cfg.MqttPort))
	g.Go(func() error {
		return proxyMQTT(ctx, cfg, logger, h)
	})

	logger.Info(fmt.Sprintf("Starting MQTT over WS  proxy on port %s", cfg.HttpPort))
	g.Go(func() error {
		return proxyWS(ctx, cfg, logger, h)
	})

	g.Go(func() error {
		if sig := errors.SignalHandler(ctx); sig != nil {
			cancel()
			logger.Info(fmt.Sprintf("mProxy shutdown by signal: %s", sig))
		}
		return nil
	})

	if err := g.Wait(); err != nil {
		logger.Error(fmt.Sprintf("mProxy terminated: %s", err))
	}
}

func proxyMQTT(ctx context.Context, cfg config, logger mflog.Logger, handler session.Handler) error {
	address := fmt.Sprintf(":%s", cfg.MqttPort)
	target := fmt.Sprintf("%s:%s", cfg.MqttTargetHost, cfg.MqttTargetPort)
	mp := mp.New(address, target, handler, logger)

	errCh := make(chan error)
	go func() {
		errCh <- mp.Listen()
	}()

	select {
	case <-ctx.Done():
		logger.Info(fmt.Sprintf("proxy MQTT shutdown at %s", target))
		return nil
	case err := <-errCh:
		return err
	}
}

func proxyWS(ctx context.Context, cfg config, logger mflog.Logger, handler session.Handler) error {
	target := fmt.Sprintf("%s:%s", cfg.HttpTargetHost, cfg.HttpTargetPort)
	wp := ws.New(target, cfg.HttpTargetPath, "ws", handler, logger)
	http.Handle("/mqtt", wp.Handler())

	errCh := make(chan error)

	go func() {
		errCh <- wp.Listen(cfg.HttpPort)
	}()

	select {
	case <-ctx.Done():
		logger.Info(fmt.Sprintf("proxy MQTT WS shutdown at %s", target))
		return nil
	case err := <-errCh:
		return err
	}
}

func healthcheck(cfg config) func() error {
	return func() error {
		res, err := http.Get(cfg.MqttTargetHealthCheck)
		if err != nil {
			return err
		}
		defer res.Body.Close()
		body, err := io.ReadAll(res.Body)
		if err != nil {
			return err
		}
		if res.StatusCode != http.StatusOK {
			return errors.New(string(body))
		}
		return nil
	}
}
