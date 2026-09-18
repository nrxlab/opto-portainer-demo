package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	mqtt "github.com/eclipse/paho.mqtt.golang"
)

type Config struct {
	Host           string
	APIKey         string
	Device         string
	Module         int
	AnalogChannels int
	PollInterval   time.Duration
	TLSInsecure    bool
	DiscoverOnly   bool
	IncludeRaw     bool
	OutputMode     string
	MQTTURL        string
	MQTTTopic      string
	MQTTClientID   string
	MQTTUsername   string
	MQTTPassword   string
	MQTTQoS        byte
	MQTTRetain     bool
}

type AnalogChannelValue struct {
	Value        float64 `json:"value"`
	QualityError bool    `json:"qualityError"`
}

type AnalogModuleValues struct {
	ModuleIndex   float64              `json:"moduleIndex"`
	ChannelValues []AnalogChannelValue `json:"channelValues"`
}

type DigitalChannelValue struct {
	State        bool `json:"state"`
	OnLatch      bool `json:"onLatch"`
	OffLatch     bool `json:"offLatch"`
	QualityError bool `json:"qualityError"`
}

type DigitalModuleValues struct {
	ModuleIndex   float64               `json:"moduleIndex"`
	ChannelValues []DigitalChannelValue `json:"channelValues"`
}

type ChannelConfig struct {
	ChannelIndex int    `json:"channelIndex"`
	ChannelType  int64  `json:"channelType"`
	Name         string `json:"name"`
	Unit         string `json:"unit"`
	PublicAccess struct {
		ModelType string `json:"modelType"`
	} `json:"publicAccessAttributes"`
}

type FieldValue struct {
	Channel      int    `json:"channel"`
	Kind         string `json:"kind"`
	Value        any    `json:"value"`
	Unit         string `json:"unit,omitempty"`
	QualityError bool   `json:"qualityError"`
	OnLatch      *bool  `json:"onLatch,omitempty"`
	OffLatch     *bool  `json:"offLatch,omitempty"`
	ChannelType  string `json:"channelType"`
}

type Sample struct {
	Timestamp time.Time             `json:"timestamp"`
	Host      string                `json:"host"`
	Device    string                `json:"device"`
	Module    int                   `json:"module"`
	Fields    map[string]FieldValue `json:"fields,omitempty"`
	Analog    *AnalogModuleValues   `json:"analog,omitempty"`
	Digital   *DigitalModuleValues  `json:"digital,omitempty"`
	Errors    []string              `json:"errors,omitempty"`
}

type Output interface {
	Write([]byte) error
	Close()
}

type StdoutOutput struct{}

func (StdoutOutput) Write(b []byte) error { _, err := fmt.Println(string(b)); return err }
func (StdoutOutput) Close()               {}

type MultiOutput []Output

func (m MultiOutput) Write(b []byte) error {
	for _, out := range m {
		if err := out.Write(b); err != nil {
			return err
		}
	}
	return nil
}
func (m MultiOutput) Close() {
	for _, out := range m {
		out.Close()
	}
}

type MQTTOutput struct {
	client mqtt.Client
	topic  string
	qos    byte
	retain bool
}

func NewMQTTOutput(cfg Config) (*MQTTOutput, error) {
	if cfg.MQTTURL == "" || cfg.MQTTTopic == "" {
		return nil, errors.New("MQTT_URL and MQTT_TOPIC are required when OUTPUT_MODE=mqtt or both")
	}
	opts := mqtt.NewClientOptions()
	opts.AddBroker(cfg.MQTTURL)
	opts.SetClientID(cfg.MQTTClientID)
	opts.SetAutoReconnect(true)
	opts.SetConnectRetry(true)
	opts.SetConnectRetryInterval(5 * time.Second)
	if cfg.MQTTUsername != "" {
		opts.SetUsername(cfg.MQTTUsername)
		opts.SetPassword(cfg.MQTTPassword)
	}
	opts.OnConnect = func(mqtt.Client) { log.Printf("mqtt connected: %s", cfg.MQTTURL) }
	opts.OnConnectionLost = func(_ mqtt.Client, err error) { log.Printf("mqtt connection lost: %v", err) }

	client := mqtt.NewClient(opts)
	tok := client.Connect()
	if !tok.WaitTimeout(15 * time.Second) {
		return nil, errors.New("mqtt connect timed out")
	}
	if err := tok.Error(); err != nil {
		return nil, err
	}
	return &MQTTOutput{client: client, topic: cfg.MQTTTopic, qos: cfg.MQTTQoS, retain: cfg.MQTTRetain}, nil
}

func (m *MQTTOutput) Write(b []byte) error {
	tok := m.client.Publish(m.topic, m.qos, m.retain, b)
	if !tok.WaitTimeout(5 * time.Second) {
		return errors.New("mqtt publish timed out")
	}
	return tok.Error()
}
func (m *MQTTOutput) Close() { m.client.Disconnect(250) }

func main() {
	cfg := loadConfig()
	if cfg.Host == "" || cfg.APIKey == "" {
		log.Fatal("OPTO_HOST and OPTO_API_KEY are required")
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	client := &http.Client{
		Timeout:   10 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: cfg.TLSInsecure}}, //nolint:gosec -- field device commonly uses self-signed certs; controlled by OPTO_TLS_INSECURE
	}

	collector := Collector{cfg: cfg, client: client}

	if cfg.DiscoverOnly {
		if err := collector.discover(ctx); err != nil {
			log.Fatal(err)
		}
		return
	}

	configs, err := collector.channelConfigs(ctx, 10)
	if err != nil {
		log.Printf("warning: channel config discovery failed; samples will include raw analog/digital arrays only: %v", err)
	} else {
		collector.configs = configs
	}

	out, err := buildOutput(cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer out.Close()

	ticker := time.NewTicker(cfg.PollInterval)
	defer ticker.Stop()

	collector.pollAndWrite(ctx, out)
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			collector.pollAndWrite(ctx, out)
		}
	}
}

func buildOutput(cfg Config) (Output, error) {
	switch strings.ToLower(cfg.OutputMode) {
	case "stdout", "":
		return StdoutOutput{}, nil
	case "mqtt":
		return NewMQTTOutput(cfg)
	case "both":
		mqttOut, err := NewMQTTOutput(cfg)
		if err != nil {
			return nil, err
		}
		return MultiOutput{StdoutOutput{}, mqttOut}, nil
	default:
		return nil, fmt.Errorf("unsupported OUTPUT_MODE %q; use stdout, mqtt, or both", cfg.OutputMode)
	}
}

type Collector struct {
	cfg     Config
	client  *http.Client
	configs []ChannelConfig
}

func (c Collector) discover(ctx context.Context) error {
	for _, path := range []string{
		fmt.Sprintf("/manage/api/v1/io/%s/info", c.cfg.Device),
		fmt.Sprintf("/manage/api/v1/io/%s/modules/info", c.cfg.Device),
		fmt.Sprintf("/manage/api/v1/io/%s/modules/type", c.cfg.Device),
	} {
		var raw any
		if err := c.getJSON(ctx, path, &raw); err != nil {
			return err
		}
		b, _ := json.MarshalIndent(raw, "", "  ")
		fmt.Printf("=== %s ===\n%s\n", path, string(b))
	}
	return nil
}

func (c Collector) pollAndWrite(ctx context.Context, out Output) {
	s := Sample{Timestamp: time.Now().UTC(), Host: c.cfg.Host, Device: c.cfg.Device, Module: c.cfg.Module}

	var analog AnalogModuleValues
	analogPath := fmt.Sprintf("/manage/api/v1/io/%s/modules/%d/analog/values?channels=%d", c.cfg.Device, c.cfg.Module, c.cfg.AnalogChannels)
	if err := c.getJSON(ctx, analogPath, &analog); err != nil {
		s.Errors = append(s.Errors, "analog: "+err.Error())
	} else if c.cfg.IncludeRaw {
		s.Analog = &analog
	}

	var digital DigitalModuleValues
	digitalPath := fmt.Sprintf("/manage/api/v1/io/%s/modules/%d/digital/values", c.cfg.Device, c.cfg.Module)
	if err := c.getJSON(ctx, digitalPath, &digital); err != nil {
		s.Errors = append(s.Errors, "digital: "+err.Error())
	} else if c.cfg.IncludeRaw {
		s.Digital = &digital
	}

	s.Fields = buildFields(c.configs, &analog, &digital)

	b, err := json.Marshal(s)
	if err != nil {
		log.Printf("marshal sample: %v", err)
		return
	}
	if err := out.Write(b); err != nil {
		log.Printf("output write failed: %v", err)
	}
}

func (c Collector) channelConfigs(ctx context.Context, channels int) ([]ChannelConfig, error) {
	configs := make([]ChannelConfig, 0, channels)
	for ch := 0; ch < channels; ch++ {
		path := fmt.Sprintf("/manage/api/v1/io/%s/modules/%d/channels/%d/config", c.cfg.Device, c.cfg.Module, ch)
		var cfg ChannelConfig
		if err := c.getJSON(ctx, path, &cfg); err != nil {
			return configs, err
		}
		configs = append(configs, cfg)
	}
	return configs, nil
}

func buildFields(configs []ChannelConfig, analog *AnalogModuleValues, digital *DigitalModuleValues) map[string]FieldValue {
	fields := make(map[string]FieldValue)
	for _, cfg := range configs {
		name := cfg.Name
		if name == "" {
			name = fmt.Sprintf("channel_%d", cfg.ChannelIndex)
		}
		if _, exists := fields[name]; exists {
			name = fmt.Sprintf("%s_ch%d", name, cfg.ChannelIndex)
		}
		fv := FieldValue{Channel: cfg.ChannelIndex, Unit: cfg.Unit, ChannelType: fmt.Sprintf("0x%08X", uint32(cfg.ChannelType))}
		switch cfg.PublicAccess.ModelType {
		case "AnalogPublicAccessAttributes":
			if analog == nil || cfg.ChannelIndex >= len(analog.ChannelValues) {
				continue
			}
			v := analog.ChannelValues[cfg.ChannelIndex]
			fv.Kind = "analog"
			fv.Value = v.Value
			fv.QualityError = v.QualityError
		case "DigitalPublicAccessAttributes":
			if digital == nil || cfg.ChannelIndex >= len(digital.ChannelValues) {
				continue
			}
			v := digital.ChannelValues[cfg.ChannelIndex]
			fv.Kind = "digital"
			fv.Value = v.State
			fv.QualityError = v.QualityError
			fv.OnLatch = &v.OnLatch
			fv.OffLatch = &v.OffLatch
		default:
			continue
		}
		fields[name] = fv
	}
	return fields
}

func (c Collector) getJSON(ctx context.Context, path string, out any) error {
	url := "https://" + strings.TrimRight(c.cfg.Host, "/") + path
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	req.Header.Set("apiKey", c.cfg.APIKey)
	req.Header.Set("Accept", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return fmt.Errorf("GET %s: HTTP %d: %s", path, resp.StatusCode, strings.TrimSpace(string(body)))
	}
	if len(body) == 0 {
		return errors.New("empty response")
	}
	if err := json.Unmarshal(body, out); err != nil {
		return fmt.Errorf("decode %s: %w: %s", path, err, strings.TrimSpace(string(body)))
	}
	return nil
}

func loadConfig() Config {
	var cfg Config
	flag.StringVar(&cfg.Host, "host", env("OPTO_HOST", ""), "groov RIO hostname or IP")
	flag.StringVar(&cfg.APIKey, "api-key", env("OPTO_API_KEY", ""), "groov Manage API key")
	flag.StringVar(&cfg.Device, "device", env("OPTO_DEVICE", "local"), "I/O device name; use local for built-in I/O")
	flag.IntVar(&cfg.Module, "module", envInt("OPTO_MODULE", 0), "module index")
	flag.IntVar(&cfg.AnalogChannels, "analog-channels", envInt("OPTO_ANALOG_CHANNELS", 8), "number of analog channels to request")
	flag.DurationVar(&cfg.PollInterval, "poll-interval", envDurationMS("OPTO_POLL_INTERVAL_MS", 500*time.Millisecond), "poll interval")
	flag.BoolVar(&cfg.TLSInsecure, "tls-insecure", envBool("OPTO_TLS_INSECURE", true), "skip TLS certificate verification")
	flag.BoolVar(&cfg.DiscoverOnly, "discover", envBool("OPTO_DISCOVER_ONLY", false), "print info/modules and exit")
	flag.BoolVar(&cfg.IncludeRaw, "include-raw", envBool("OPTO_INCLUDE_RAW", false), "include raw packed analog/digital arrays in each sample")
	flag.StringVar(&cfg.OutputMode, "output-mode", env("OUTPUT_MODE", "stdout"), "output mode: stdout, mqtt, or both")
	flag.StringVar(&cfg.MQTTURL, "mqtt-url", env("MQTT_URL", "tcp://mosquitto:1883"), "MQTT broker URL")
	flag.StringVar(&cfg.MQTTTopic, "mqtt-topic", env("MQTT_TOPIC", "opto/rio/grv-r7-mm1001-10/state"), "MQTT topic for state payloads")
	flag.StringVar(&cfg.MQTTClientID, "mqtt-client-id", env("MQTT_CLIENT_ID", "opto-rio-rest-collector"), "MQTT client ID")
	flag.StringVar(&cfg.MQTTUsername, "mqtt-username", env("MQTT_USERNAME", ""), "MQTT username")
	flag.StringVar(&cfg.MQTTPassword, "mqtt-password", env("MQTT_PASSWORD", ""), "MQTT password")
	flag.BoolVar(&cfg.MQTTRetain, "mqtt-retain", envBool("MQTT_RETAIN", false), "publish retained MQTT messages")
	flag.Parse()
	cfg.MQTTQoS = byte(envInt("MQTT_QOS", 0))
	return cfg
}

func env(k, d string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return d
}

func envInt(k string, d int) int {
	if v := os.Getenv(k); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return d
}

func envBool(k string, d bool) bool {
	if v := os.Getenv(k); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return d
}

func envDurationMS(k string, d time.Duration) time.Duration {
	if v := os.Getenv(k); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return time.Duration(i) * time.Millisecond
		}
		if dur, err := time.ParseDuration(v); err == nil {
			return dur
		}
	}
	return d
}
