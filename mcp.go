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
	k6metrics "go.k6.io/k6/metrics"
	"golang.org/x/oauth2"

	"github.com/grafana/xk6-mcp/metrics"
)

func init() {
	modules.Register("k6/x/mcp", New())
}

// MCP is the root module struct
type (
	RootModule struct{}

	// MCPInstance represents an instance of the MCP module
	MCPInstance struct {
		vu       modules.VU
		logger   logrus.FieldLogger
		registry *k6metrics.Registry
	}

	// ClientConfig represents the configuration for the MCP client
	ClientConfig struct {
		// Stdio
		Path  string
		Args  []string
		Env   map[string]string
		Debug bool

		// SSE and Streamable HTTP
		BaseURL string
		Auth    AuthConfig
	}

	AuthConfig struct {
		BearerToken string
	}
)

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

	return &MCPInstance{
		vu:       vu,
		logger:   logger,
		registry: env.Registry,
	}
}

// Client wraps an MCP client session
type Client struct {
	ctx     context.Context
	session *mcp.ClientSession
	metrics *metrics.K6Metrics
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

	mcpMetrics := metrics.NewK6Metrics(
		m.registry,
		m.vu.State().Samples,
		m.vu.State().Tags.GetCurrentValues(),
	)

	return rt.ToValue(&Client{
		ctx:     m.vu.Context(),
		session: client.session,
		metrics: mcpMetrics,
	}).ToObject(rt)
}

func (m *MCPInstance) newSSEClient(c sobek.ConstructorCall, rt *sobek.Runtime) *sobek.Object {
	var cfg ClientConfig
	if err := rt.ExportTo(c.Argument(0), &cfg); err != nil {
		common.Throw(rt, fmt.Errorf("invalid config: %w", err))
	}

	transport := &mcp.SSEClientTransport{
		Endpoint:   cfg.BaseURL,
		HTTPClient: m.newk6HTTPClient(cfg),
	}

	clientObj := m.connect(rt, transport, true)
	var client *Client
	if err := rt.ExportTo(clientObj, &client); err != nil {
		common.Throw(rt, fmt.Errorf("failed to extract Client: %w", err))
	}

	mcpMetrics := metrics.NewK6Metrics(
		m.registry,
		m.vu.State().Samples,
		m.vu.State().Tags.GetCurrentValues(),
	)

	return rt.ToValue(&Client{
		ctx:     m.vu.Context(),
		session: client.session,
		metrics: mcpMetrics,
	}).ToObject(rt)
}

func (m *MCPInstance) newStreamableHTTPClient(c sobek.ConstructorCall, rt *sobek.Runtime) *sobek.Object {
	var cfg ClientConfig
	if err := rt.ExportTo(c.Argument(0), &cfg); err != nil {
		common.Throw(rt, fmt.Errorf("invalid config: %w", err))
	}

	transport := &mcp.StreamableClientTransport{
		Endpoint:   cfg.BaseURL,
		HTTPClient: m.newk6HTTPClient(cfg),
	}

	clientObj := m.connect(rt, transport, false)
	var client *Client
	if err := rt.ExportTo(clientObj, &client); err != nil {
		common.Throw(rt, fmt.Errorf("failed to extract Client: %w", err))
	}

	mcpMetrics := metrics.NewK6Metrics(
		m.registry,
		m.vu.State().Samples,
		m.vu.State().Tags.GetCurrentValues(),
	)

	return rt.ToValue(&Client{
		ctx:     m.vu.Context(),
		session: client.session,
		metrics: mcpMetrics,
	}).ToObject(rt)
}

func (m *MCPInstance) newk6HTTPClient(cfg ClientConfig) *http.Client {
	var tlsConfig *tls.Config
	if m.vu.State() != nil && m.vu.State().TLSConfig != nil {
		tlsConfig = m.vu.State().TLSConfig.Clone()
		tlsConfig.NextProtos = []string{"http/1.1"}
	}

	transport := http.Transport{
		Proxy:           http.ProxyFromEnvironment,
		TLSClientConfig: tlsConfig,
	}
	if m.vu.State() != nil {
		transport.DisableKeepAlives = m.vu.State().Options.NoConnectionReuse.ValueOrZero() || m.vu.State().Options.NoVUConnectionReuse.ValueOrZero()
	}
	if m.vu.State().Dialer != nil {
		transport.DialContext = m.vu.State().Dialer.DialContext
	}

	httpClient := &http.Client{
		Transport: &transport,
	}

	if cfg.Auth.BearerToken != "" {
		ctx := context.Background()

		ctx = context.WithValue(ctx, oauth2.HTTPClient, httpClient)

		token := oauth2.Token{
			AccessToken: cfg.Auth.BearerToken,
		}
		tokenSource := oauth2.StaticTokenSource(&token)

		httpClient = oauth2.NewClient(ctx, tokenSource)
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

	c.metrics.Push(c.ctx, "ListAllTools", time.Since(start), err)
	if err != nil {
		return nil, fmt.Errorf("failed to list tools: %w", err)
	}

	return &ListAllToolsResult{
		Tools: allTools,
	}, nil
}

func (c *Client) CallTool(r mcp.CallToolParams) (*mcp.CallToolResult, error) {
	start := time.Now()
	result, err := c.session.CallTool(c.ctx, &r)
	c.metrics.Push(c.ctx, "CallTool", time.Since(start), err)
	return result, err
}

func (c *Client) ListResources(r mcp.ListResourcesParams) (*mcp.ListResourcesResult, error) {
	start := time.Now()
	res, err := c.session.ListResources(context.Background(), &r)
	c.metrics.Push(c.ctx, "ListResources", time.Since(start), err)
	return res, err
}

func (c *Client) ReadResource(r mcp.ReadResourceParams) (*mcp.ReadResourceResult, error) {
	start := time.Now()
	res, err := c.session.ReadResource(context.Background(), &r)
	c.metrics.Push(c.ctx, "ReadResource", time.Since(start), err)
	return res, err
}

func (c *Client) ListPrompts(r mcp.ListPromptsParams) (*mcp.ListPromptsResult, error) {
	start := time.Now()
	res, err := c.session.ListPrompts(context.Background(), &r)
	c.metrics.Push(c.ctx, "ListPrompts", time.Since(start), err)
	return res, err
}

func (c *Client) GetPrompt(r mcp.GetPromptParams) (*mcp.GetPromptResult, error) {
	start := time.Now()
	res, err := c.session.GetPrompt(context.Background(), &r)
	c.metrics.Push(c.ctx, "GetPrompt", time.Since(start), err)
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

	c.metrics.Push(c.ctx, "ListAllResources", time.Since(start), err)
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

	c.metrics.Push(c.ctx, "ListAllPrompts", time.Since(start), err)
	if err != nil {
		return nil, fmt.Errorf("failed to list prompts: %w", err)
	}

	return &ListAllPromptsResult{
		Prompts: allPrompts,
	}, nil
}
