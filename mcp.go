package mcp

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"time"

	"github.com/grafana/sobek"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/sirupsen/logrus"
	"go.k6.io/k6/js/common"
	"go.k6.io/k6/js/modules"
	"go.k6.io/k6/lib"
	"go.k6.io/k6/metrics"
)

func init() {
	modules.Register("k6/x/mcp", New())
}

// MCP is the root module struct
type (
	RootModule struct{}
)

var mcp_metrics *mcpMetrics

// New returns a pointer to a new RootModule instance.
func New() *RootModule {
	return &RootModule{}
}

var (
	_ modules.Instance = &MCPInstance{}
	_ modules.Module   = &RootModule{}
)

// NewModuleInstance initializes a new module instance
func (*RootModule) NewModuleInstance(vu modules.VU) modules.Instance {
	env := vu.InitEnv()

	logger := env.Logger.WithField("component", "xk6-mcp")

	mcp_request_duration, _ := env.Registry.NewMetric("mcp_request_duration", metrics.Trend)
	mcp_request_count, _ := env.Registry.NewMetric("mcp_request_count", metrics.Counter)
	mcp_request_errors, _ := env.Registry.NewMetric("mcp_request_errors", metrics.Counter)

	mcp_metrics = &mcpMetrics{
		RequestDuration: mcp_request_duration,
		RequestCount:    mcp_request_count,
		RequestErrors:   mcp_request_errors,
	}

	mcp_metrics.TagsAndMeta = &metrics.TagsAndMeta{
		Tags: env.Registry.RootTagSet(),
	}

	return &MCPInstance{
		vu:     vu,
		logger: logger,
	}
}

// MCPInstance represents an instance of the MCP module
type MCPInstance struct {
	vu     modules.VU
	logger logrus.FieldLogger
}

type mcpMetrics struct {
	RequestDuration *metrics.Metric
	RequestCount    *metrics.Metric
	RequestErrors   *metrics.Metric

	TagsAndMeta *metrics.TagsAndMeta
}

// ClientConfig represents the configuration for the MCP client
type ClientConfig struct {
	// Stdio
	Path  string
	Args  []string
	Env   map[string]string
	Debug bool

	// SSE and Streamable HTTP
	BaseURL string
	Timeout time.Duration
}

// Client wraps an MCP client session
type Client struct {
	session *mcp.ClientSession

	k6_state *lib.State
}

// Exports defines the JavaScript-accessible functions
func (m *MCPInstance) Exports() modules.Exports {
	return modules.Exports{
		Named: map[string]interface{}{
			"StdioClient":          m.newStdioClient,
			"SSEClient":            m.newSSEClient,
			"StreamableHTTPClient": m.newStreamableHTTPClient,
		},
	}
}

func (m *MCPInstance) newStdioClient(c sobek.ConstructorCall, rt *sobek.Runtime) *sobek.Object {
	var cfg ClientConfig
	if err := rt.ExportTo(c.Argument(0), &cfg); err != nil {
		common.Throw(rt, fmt.Errorf("invalid config: %w", err))
	}

	cmd := exec.Command(cfg.Path, cfg.Args...)
	for k, v := range cfg.Env {
		cmd.Env = append(cmd.Env, fmt.Sprintf("%s=%s", k, v))
	}

	if cfg.Debug {
		cmd.Stderr = os.Stderr
	}

	transport := &mcp.CommandTransport{
		Command: cmd,
	}

	clientObj := m.connect(rt, transport, false)
	var client *Client
	if err := rt.ExportTo(clientObj, &client); err != nil {
		common.Throw(rt, fmt.Errorf("failed to extract Client: %w", err))
	}

	return rt.ToValue(&Client{
		session:  client.session,
		k6_state: m.vu.State(),
	}).ToObject(rt)
}

func (m *MCPInstance) newSSEClient(c sobek.ConstructorCall, rt *sobek.Runtime) *sobek.Object {
	var cfg ClientConfig
	if err := rt.ExportTo(c.Argument(0), &cfg); err != nil {
		common.Throw(rt, fmt.Errorf("invalid config: %w", err))
	}

	transport := &mcp.SSEClientTransport{
		Endpoint:   cfg.BaseURL,
		HTTPClient: m.newk6HTTPClient(),
	}

	clientObj := m.connect(rt, transport, true)
	var client *Client
	if err := rt.ExportTo(clientObj, &client); err != nil {
		common.Throw(rt, fmt.Errorf("failed to extract Client: %w", err))
	}

	return rt.ToValue(&Client{
		session:  client.session,
		k6_state: m.vu.State(),
	}).ToObject(rt)
}

func (m *MCPInstance) newStreamableHTTPClient(c sobek.ConstructorCall, rt *sobek.Runtime) *sobek.Object {
	var cfg ClientConfig
	if err := rt.ExportTo(c.Argument(0), &cfg); err != nil {
		common.Throw(rt, fmt.Errorf("invalid config: %w", err))
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint:   cfg.BaseURL,
		HTTPClient: m.newk6HTTPClient(),
	}

	clientObj := m.connect(rt, transport, false)
	var client *Client
	if err := rt.ExportTo(clientObj, &client); err != nil {
		common.Throw(rt, fmt.Errorf("failed to extract Client: %w", err))
	}

	return rt.ToValue(&Client{
		session:  client.session,
		k6_state: m.vu.State(),
	}).ToObject(rt)
}

func (m *MCPInstance) newk6HTTPClient() *http.Client {
	var tlsConfig *tls.Config
	if m.vu.State().TLSConfig != nil {
		tlsConfig = m.vu.State().TLSConfig.Clone()
		tlsConfig.NextProtos = []string{"http/1.1"}
	}

	httpClient := &http.Client{
		Transport: &http.Transport{
			DialContext:       m.vu.State().Dialer.DialContext,
			Proxy:             http.ProxyFromEnvironment,
			TLSClientConfig:   tlsConfig,
			DisableKeepAlives: m.vu.State().Options.NoConnectionReuse.ValueOrZero() || m.vu.State().Options.NoVUConnectionReuse.ValueOrZero(),
		},
	}

	return httpClient
}

func (m *MCPInstance) connect(rt *sobek.Runtime, transport mcp.Transport, isSSE bool) *sobek.Object {
	var ctx context.Context
	var cancel context.CancelFunc
	if isSSE {
		ctx = context.Background()
		cancel = func() {}
	} else {
		ctx, cancel = context.WithTimeout(context.Background(), 30*time.Second)
	}
	defer cancel()

	client := mcp.NewClient(&mcp.Implementation{Name: "k6", Version: "1.0.0"}, nil)
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		common.Throw(rt, fmt.Errorf("connection error: %w", err))
	}

	return rt.ToValue(&Client{session: session}).ToObject(rt)
}

func (c *Client) Ping() bool {
	err := c.session.Ping(context.Background(), &mcp.PingParams{})
	return err == nil
}

func (c *Client) ListTools(r mcp.ListToolsParams) (*mcp.ListToolsResult, error) {
	return c.session.ListTools(context.Background(), &r)
}

type ListAllToolsParams struct {
	Meta mcp.Meta
}

type ListAllToolsResult struct {
	Tools []mcp.Tool
}

func (c *Client) ListAllTools(r ListAllToolsParams) (*ListAllToolsResult, error) {
	if r.Meta == nil {
		r.Meta = mcp.Meta{}
	}

	var allTools []mcp.Tool
	cursor := ""
	start := time.Now()
	var err error
	for {
		params := &mcp.ListToolsParams{Meta: r.Meta}
		if cursor != "" {
			params.Cursor = cursor
		}
		var result *mcp.ListToolsResult
		result, err = c.session.ListTools(context.Background(), params)
		if err != nil {
			break
		}

		for _, t := range result.Tools {
			if t != nil {
				allTools = append(allTools, *t)
			}
		}

		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}

	pushRequestMetrics(c, "ListAllTools", time.Since(start), err)
	if err != nil {
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}

	return &ListAllToolsResult{
		Tools: allTools,
	}, nil
}

func (c *Client) CallTool(r mcp.CallToolParams) (*mcp.CallToolResult, error) {
	start := time.Now()
	result, err := c.session.CallTool(context.Background(), &r)
	pushRequestMetrics(c, "CallTool", time.Since(start), err)
	return result, err
}

func (c *Client) ListResources(r mcp.ListResourcesParams) (*mcp.ListResourcesResult, error) {
	start := time.Now()
	res, err := c.session.ListResources(context.Background(), &r)
	pushRequestMetrics(c, "ListResources", time.Since(start), err)
	return res, err
}

func (c *Client) ReadResource(r mcp.ReadResourceParams) (*mcp.ReadResourceResult, error) {
	start := time.Now()
	res, err := c.session.ReadResource(context.Background(), &r)
	pushRequestMetrics(c, "ReadResource", time.Since(start), err)
	return res, err
}

func (c *Client) ListPrompts(r mcp.ListPromptsParams) (*mcp.ListPromptsResult, error) {
	start := time.Now()
	res, err := c.session.ListPrompts(context.Background(), &r)
	pushRequestMetrics(c, "ListPrompts", time.Since(start), err)
	return res, err
}

func (c *Client) GetPrompt(r mcp.GetPromptParams) (*mcp.GetPromptResult, error) {
	start := time.Now()
	res, err := c.session.GetPrompt(context.Background(), &r)
	pushRequestMetrics(c, "GetPrompt", time.Since(start), err)
	return res, err
}

type ListAllResourcesParams struct {
	Meta mcp.Meta
}

type ListAllResourcesResult struct {
	Resources []mcp.Resource
}

func (c *Client) ListAllResources(r ListAllResourcesParams) (*ListAllResourcesResult, error) {
	if r.Meta == nil {
		r.Meta = mcp.Meta{}
	}

	var allResources []mcp.Resource
	cursor := ""
	start := time.Now()
	var err error
	for {
		params := &mcp.ListResourcesParams{Meta: r.Meta}
		if cursor != "" {
			params.Cursor = cursor
		}
		var result *mcp.ListResourcesResult
		result, err = c.session.ListResources(context.Background(), params)
		if err != nil {
			break
		}

		for _, res := range result.Resources {
			if res != nil {
				allResources = append(allResources, *res)
			}
		}

		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}

	pushRequestMetrics(c, "ListAllResources", time.Since(start), err)
	if err != nil {
		return nil, fmt.Errorf("failed to list resources: %w", err)
	}

	return &ListAllResourcesResult{
		Resources: allResources,
	}, nil
}

type ListAllPromptsParams struct {
	Meta mcp.Meta
}

type ListAllPromptsResult struct {
	Prompts []mcp.Prompt
}

func (c *Client) ListAllPrompts(r ListAllPromptsParams) (*ListAllPromptsResult, error) {
	if r.Meta == nil {
		r.Meta = mcp.Meta{}
	}

	var allPrompts []mcp.Prompt
	cursor := ""
	start := time.Now()
	var err error
	for {
		params := &mcp.ListPromptsParams{Meta: r.Meta}
		if cursor != "" {
			params.Cursor = cursor
		}
		var result *mcp.ListPromptsResult
		result, err = c.session.ListPrompts(context.Background(), params)
		if err != nil {
			break
		}

		for _, p := range result.Prompts {
			if p != nil {
				allPrompts = append(allPrompts, *p)
			}
		}

		if result.NextCursor == "" {
			break
		}
		cursor = result.NextCursor
	}

	pushRequestMetrics(c, "ListAllPrompts", time.Since(start), err)
	if err != nil {
		return nil, fmt.Errorf("failed to list prompts: %w", err)
	}

	return &ListAllPromptsResult{
		Prompts: allPrompts,
	}, nil
}

func pushRequestMetrics(client *Client, method string, duration time.Duration, err error) {
	metrics.PushIfNotDone(context.Background(), client.k6_state.Samples, metrics.Sample{
		TimeSeries: metrics.TimeSeries{
			Metric: mcp_metrics.RequestDuration,
			Tags: client.k6_state.Tags.GetCurrentValues().Tags.With(
				"method", method,
			),
		},
		Time:  time.Now(),
		Value: float64(duration) / float64(time.Millisecond),
	})

	metrics.PushIfNotDone(context.Background(), client.k6_state.Samples, metrics.Sample{
		TimeSeries: metrics.TimeSeries{
			Metric: mcp_metrics.RequestCount,
			Tags: client.k6_state.Tags.GetCurrentValues().Tags.With(
				"method", method,
			),
		},
		Time:  time.Now(),
		Value: 1,
	})

	if err != nil {
		metrics.PushIfNotDone(context.Background(), client.k6_state.Samples, metrics.Sample{
			TimeSeries: metrics.TimeSeries{
				Metric: mcp_metrics.RequestErrors,
				Tags: client.k6_state.Tags.GetCurrentValues().Tags.With(
					"method", method,
				),
			},
			Time:  time.Now(),
			Value: 1,
		})
	}
}
