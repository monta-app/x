// Copyright © 2023 Ory Corp
// SPDX-License-Identifier: Apache-2.0

package configx

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"reflect"
	"strings"
	"sync"
	"time"

	"github.com/inhies/go-bytesize"
	"github.com/knadh/koanf"
	"github.com/knadh/koanf/parsers/json"
	"github.com/knadh/koanf/providers/posflag"
	"github.com/pkg/errors"
	"github.com/rs/cors"
	"github.com/sirupsen/logrus"
	"github.com/spf13/pflag"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/trace"

	"github.com/ory/jsonschema/v3"
	"github.com/ory/x/jsonschemax"
	"github.com/ory/x/logrusx"
	"github.com/ory/x/otelx"
	"github.com/ory/x/watcherx"
)

type tuple struct {
	Key   string
	Value interface{}
}

const tracingComponent = "github.com/ory/x/configx"

type Provider struct {
	l sync.RWMutex
	*koanf.Koanf
	immutables []string

	cancel context.CancelFunc

	schema                   []byte
	flags                    *pflag.FlagSet
	validator                *jsonschema.Schema
	onChanges                []func(watcherx.Event, error)
	onValidationError        func(k *koanf.Koanf, err error)
	excludeFieldsFromTracing []string
	tracer                   trace.Tracer

	forcedValues []tuple
	baseValues   []tuple
	files        []string

	skipValidation    bool
	disableEnvLoading bool

	logger *logrusx.Logger

	providers     []koanf.Provider
	userProviders []koanf.Provider
}

const (
	FlagConfig = "config"
	Delimiter  = "."
)

// RegisterConfigFlag registers the "--config" flag on pflag.FlagSet.
func RegisterConfigFlag(flags *pflag.FlagSet, fallback []string) {
	flags.StringSliceP(FlagConfig, "c", fallback, "Config files to load, overwriting in the order specified.")
}

// New creates a new provider instance or errors.
// Configuration values are loaded in the following order:
//
// 1. Defaults from the JSON Schema
// 2. Config files (yaml, yml, toml, json)
// 3. Command line flags
// 4. Environment variables
//
// ctx is used for cancelation in case a schema is loaded from a URL. Background
// reloading of files is enabled automatically. Call Close on the returned
// Provider to stop the background reloading.
func New(ctx context.Context, schema []byte, modifiers ...OptionModifier) (*Provider, error) {
	validator, err := getSchema(ctx, schema)
	if err != nil {
		return nil, err
	}

	l := logrus.New()
	l.Out = io.Discard

	p := &Provider{
		schema:                   schema,
		validator:                validator,
		onValidationError:        func(k *koanf.Koanf, err error) {},
		excludeFieldsFromTracing: []string{"dsn", "secret", "password", "key"},
		logger:                   logrusx.New("discarding config logger", "", logrusx.UseLogger(l)),
		Koanf:                    koanf.NewWithConf(koanf.Conf{Delim: Delimiter, StrictMerge: true}),
	}

	for _, m := range modifiers {
		m(p)
	}

	ctx, p.cancel = context.WithCancel(context.Background())
	providers, err := p.createProviders(ctx)
	if err != nil {
		return nil, err
	}

	p.providers = providers

	k, err := p.newKoanf()
	if err != nil {
		return nil, err
	}

	p.replaceKoanf(k)
	return p, nil
}

// Close stops background reloading.
func (p *Provider) Close() {
	if p.cancel != nil {
		p.cancel()
		p.cancel = nil
	}
}

func (p *Provider) SkipValidation() bool {
	return p.skipValidation
}

func (p *Provider) createProviders(ctx context.Context) (providers []koanf.Provider, err error) {
	defaultsProvider, err := NewKoanfSchemaDefaults(p.schema, p.validator)
	if err != nil {
		return nil, err
	}
	providers = append(providers, defaultsProvider)

	// Workaround for https://github.com/knadh/koanf/pull/47
	for _, t := range p.baseValues {
		providers = append(providers, NewKoanfConfmap([]tuple{t}))
	}

	paths := p.files
	if p.flags != nil {
		p, _ := p.flags.GetStringSlice(FlagConfig)
		paths = append(paths, p...)
	}

	p.logger.WithField("files", paths).Debug("Adding config files.")

	c := make(watcherx.EventChannel)
	go p.watchForFileChanges(ctx, c)

	for _, path := range paths {
		fp, err := NewKoanfFile(ctx, path)
		if err != nil {
			return nil, err
		}

		if _, err := fp.WatchChannel(c); err != nil {
			return nil, err
		}

		providers = append(providers, fp)
	}

	providers = append(providers, p.userProviders...)

	if p.flags != nil {
		pp, err := NewPFlagProvider(p.schema, p.validator, p.flags, p.Koanf)
		if err != nil {
			return nil, err
		}
		providers = append(providers, pp)
	}

	if !p.disableEnvLoading {
		envProvider, err := NewKoanfEnv("", p.schema, p.validator)
		if err != nil {
			return nil, err
		}
		providers = append(providers, envProvider)
	}

	// Workaround for https://github.com/knadh/koanf/pull/47
	for _, t := range p.forcedValues {
		providers = append(providers, NewKoanfConfmap([]tuple{t}))
	}

	return providers, nil
}

func (p *Provider) replaceKoanf(k *koanf.Koanf) {
	p.Koanf = k
}

func (p *Provider) validate(k *koanf.Koanf) error {
	if p.skipValidation {
		return nil
	}

	out, err := k.Marshal(json.Parser())
	if err != nil {
		return errors.WithStack(err)
	}
	if err := p.validator.Validate(bytes.NewReader(out)); err != nil {
		p.onValidationError(k, err)
		return err
	}

	return nil
}

// newKoanf creates a new koanf instance with all the updated config
//
// This is unfortunately required due to several limitations / bugs in koanf:
//
// - https://github.com/knadh/koanf/issues/77
// - https://github.com/knadh/koanf/pull/47
func (p *Provider) newKoanf() (k *koanf.Koanf, err error) {
	_, span := p.startSpan(context.Background(), LoadSpanOpName)
	defer otelx.End(span, &err)

	k = koanf.New(Delimiter)

	for _, provider := range p.providers {
		// posflag.Posflag requires access to Koanf instance so we recreate the provider here which is a workaround
		// for posflag.Provider's API.
		if _, ok := provider.(*posflag.Posflag); ok {
			provider = posflag.Provider(p.flags, ".", k)
		}

		var opts []koanf.Option
		if _, ok := provider.(*Env); ok {
			opts = append(opts, koanf.WithMergeFunc(MergeAllTypes))
		}

		if err := k.Load(provider, nil, opts...); err != nil {
			return nil, err
		}
	}

	if err := p.validate(k); err != nil {
		return nil, err
	}

	return k, nil
}

// SetTracer sets the tracer.
func (p *Provider) SetTracer(_ context.Context, t *otelx.Tracer) {
	p.tracer = t.Provider().Tracer(tracingComponent)
}

func (p *Provider) startSpan(ctx context.Context, opName string) (context.Context, trace.Span) {
	tracer := otel.Tracer(tracingComponent)
	if p.tracer != nil {
		tracer = p.tracer
	}
	return tracer.Start(ctx, opName)
}

func (p *Provider) runOnChanges(e watcherx.Event, err error) {
	for k := range p.onChanges {
		p.onChanges[k](e, err)
	}
}

func (p *Provider) reload(e watcherx.Event) {
	p.l.Lock()

	var err error
	defer func() {
		// we first want to unlock and then runOnChanges, so that the callbacks can actually use the Provider
		p.l.Unlock()
		p.runOnChanges(e, err)
	}()

	nk, err := p.newKoanf()
	if err != nil {
		return // unlocks & runs changes in defer
	}

	for _, key := range p.immutables {
		if !reflect.DeepEqual(p.Koanf.Get(key), nk.Get(key)) {
			err = NewImmutableError(key, fmt.Sprintf("%v", p.Koanf.Get(key)), fmt.Sprintf("%v", nk.Get(key)))
			return // unlocks & runs changes in defer
		}
	}

	p.replaceKoanf(nk)

	// unlocks & runs changes in defer
}

func (p *Provider) watchForFileChanges(ctx context.Context, c watcherx.EventChannel) {
	for {
		select {
		case <-ctx.Done():
			return
		case e := <-c:
			switch et := e.(type) {
			case *watcherx.ErrorEvent:
				p.runOnChanges(e, et)
			default:
				p.reload(e)
			}
		}
	}
}

func (p *Provider) Set(key string, value interface{}) error {
	p.l.Lock()
	defer p.l.Unlock()

	p.forcedValues = append(p.forcedValues, tuple{Key: key, Value: value})
	p.providers = append(p.providers, NewKoanfConfmap([]tuple{{Key: key, Value: value}}))

	k, err := p.newKoanf()
	if err != nil {
		return err
	}

	p.replaceKoanf(k)
	return nil
}

func (p *Provider) BoolF(key string, fallback bool) bool {
	p.l.RLock()
	defer p.l.RUnlock()

	if !p.Koanf.Exists(key) {
		return fallback
	}

	return p.Bool(key)
}

func (p *Provider) StringF(key string, fallback string) string {
	p.l.RLock()
	defer p.l.RUnlock()

	if !p.Koanf.Exists(key) {
		return fallback
	}

	return p.String(key)
}

func (p *Provider) StringsF(key string, fallback []string) (val []string) {
	p.l.RLock()
	defer p.l.RUnlock()

	if !p.Koanf.Exists(key) {
		return fallback
	}

	return p.Strings(key)
}

func (p *Provider) IntF(key string, fallback int) (val int) {
	p.l.RLock()
	defer p.l.RUnlock()

	if !p.Koanf.Exists(key) {
		return fallback
	}

	return p.Int(key)
}

func (p *Provider) Float64F(key string, fallback float64) (val float64) {
	p.l.RLock()
	defer p.l.RUnlock()

	if !p.Koanf.Exists(key) {
		return fallback
	}

	return p.Float64(key)
}

func (p *Provider) DurationF(key string, fallback time.Duration) (val time.Duration) {
	p.l.RLock()
	defer p.l.RUnlock()

	if !p.Koanf.Exists(key) {
		return fallback
	}

	return p.Duration(key)
}

func (p *Provider) ByteSizeF(key string, fallback bytesize.ByteSize) bytesize.ByteSize {
	p.l.RLock()
	defer p.l.RUnlock()

	if !p.Koanf.Exists(key) {
		return fallback
	}

	switch v := p.Koanf.Get(key).(type) {
	case string:
		// this type usually comes from user input
		dec, err := bytesize.Parse(v)
		if err != nil {
			p.logger.WithField("key", key).WithField("raw_value", v).WithError(err).Warnf("error parsing byte size value, using fallback of %s", fallback)
			return fallback
		}
		return dec
	case float64:
		// this type comes from json.Unmarshal
		return bytesize.ByteSize(v)
	case bytesize.ByteSize:
		return v
	default:
		p.logger.WithField("key", key).WithField("raw_type", fmt.Sprintf("%T", v)).WithField("raw_value", fmt.Sprintf("%+v", v)).Errorf("error converting byte size value because of unknown type, using fallback of %s", fallback)
		return fallback
	}
}

func (p *Provider) GetF(key string, fallback interface{}) (val interface{}) {
	p.l.RLock()
	defer p.l.RUnlock()

	if !p.Exists(key) {
		return fallback
	}

	return p.Get(key)
}

func (p *Provider) CORS(prefix string, defaults cors.Options) (cors.Options, bool) {
	if len(prefix) > 0 {
		prefix = strings.TrimRight(prefix, ".") + "."
	}

	return cors.Options{
		AllowedOrigins:     p.StringsF(prefix+"cors.allowed_origins", defaults.AllowedOrigins),
		AllowedMethods:     p.StringsF(prefix+"cors.allowed_methods", defaults.AllowedMethods),
		AllowedHeaders:     p.StringsF(prefix+"cors.allowed_headers", defaults.AllowedHeaders),
		ExposedHeaders:     p.StringsF(prefix+"cors.exposed_headers", defaults.ExposedHeaders),
		AllowCredentials:   p.BoolF(prefix+"cors.allow_credentials", defaults.AllowCredentials),
		OptionsPassthrough: p.BoolF(prefix+"cors.options_passthrough", defaults.OptionsPassthrough),
		MaxAge:             p.IntF(prefix+"cors.max_age", defaults.MaxAge),
		Debug:              p.BoolF(prefix+"cors.debug", defaults.Debug),
	}, p.Bool(prefix + "cors.enabled")
}

func (p *Provider) TracingConfig(serviceName string) *otelx.Config {
	return &otelx.Config{
		ServiceName:           p.StringF("tracing.service_name", serviceName),
		DeploymentEnvironment: p.StringF("tracing.deployment_environment", ""),
		Provider:              p.String("tracing.provider"),
		Providers: otelx.ProvidersConfig{
			Jaeger: otelx.JaegerConfig{
				Sampling: otelx.JaegerSampling{
					ServerURL:    p.String("tracing.providers.jaeger.sampling.server_url"),
					TraceIdRatio: p.Float64F("tracing.providers.jaeger.sampling.trace_id_ratio", 1),
				},
				LocalAgentAddress: p.String("tracing.providers.jaeger.local_agent_address"),
			},
			Zipkin: otelx.ZipkinConfig{
				ServerURL: p.String("tracing.providers.zipkin.server_url"),
				Sampling: otelx.ZipkinSampling{
					SamplingRatio: p.Float64("tracing.providers.zipkin.sampling.sampling_ratio"),
				},
			},
			OTLP: otelx.OTLPConfig{
				ServerURL: p.String("tracing.providers.otlp.server_url"),
				Insecure:  p.Bool("tracing.providers.otlp.insecure"),
				Sampling: otelx.OTLPSampling{
					SamplingRatio: p.Float64("tracing.providers.otlp.sampling.sampling_ratio"),
				},
				AuthorizationHeader: p.String("tracing.providers.otlp.authorization_header"),
			},
		},
	}
}

func (p *Provider) RequestURIF(path string, fallback *url.URL) *url.URL {
	p.l.RLock()
	defer p.l.RUnlock()

	switch t := p.Get(path).(type) {
	case *url.URL:
		return t
	case url.URL:
		return &t
	case string:
		if parsed, err := url.ParseRequestURI(t); err == nil {
			return parsed
		}
	}

	return fallback
}

func (p *Provider) URIF(path string, fallback *url.URL) *url.URL {
	p.l.RLock()
	defer p.l.RUnlock()

	switch t := p.Get(path).(type) {
	case *url.URL:
		return t
	case url.URL:
		return &t
	case string:
		if parsed, err := url.Parse(t); err == nil {
			return parsed
		}
	}

	return fallback
}

// PrintHumanReadableValidationErrors prints human readable validation errors. Duh.
func (p *Provider) PrintHumanReadableValidationErrors(w io.Writer, err error) {
	p.printHumanReadableValidationErrors(p.Koanf, w, err)
}

func (p *Provider) printHumanReadableValidationErrors(k *koanf.Koanf, w io.Writer, err error) {
	if err == nil {
		return
	}

	_, _ = fmt.Fprintln(os.Stderr, "")
	conf, innerErr := k.Marshal(json.Parser())
	if innerErr != nil {
		_, _ = fmt.Fprintf(w, "Unable to unmarshal configuration: %+v", innerErr)
	}

	jsonschemax.FormatValidationErrorForCLI(w, conf, err)
}
