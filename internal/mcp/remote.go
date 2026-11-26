package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/k8s-ai/k8s-ai/internal/config"
	"github.com/k8s-ai/k8s-ai/internal/tools"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

var modelToolName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type remoteClient struct {
	spec    config.MCPServerConfig
	session *sdk.ClientSession
}

func (c *remoteClient) Name() string { return c.spec.Name }
func (c *remoteClient) Close() error { return c.session.Close() }
func (c *remoteClient) Connect(ctx context.Context) error {
	u, err := url.Parse(c.spec.URL)
	if err != nil || u.Hostname() == "" || u.User != nil || u.Fragment != "" || (u.Scheme != "https" && !(u.Scheme == "http" && c.spec.AllowHTTP)) {
		return errors.New("invalid MCP URL or HTTP not explicitly enabled")
	}
	client := &http.Client{Transport: endpointTransport{origin: u, headers: c.spec.Headers}, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	var transport sdk.Transport
	switch c.spec.Transport {
	case "", "streamable-http":
		transport = &sdk.StreamableClientTransport{Endpoint: u.String(), HTTPClient: client, DisableStandaloneSSE: true}
	case "sse":
		transport = &sdk.SSEClientTransport{Endpoint: u.String(), HTTPClient: client}
	default:
		return errors.New("unsupported MCP transport")
	}
	c.session, err = sdk.NewClient(&sdk.Implementation{Name: "AegisOps"}, nil).Connect(ctx, transport, nil)
	if err != nil {
		return errors.New("MCP initialization failed")
	}
	return nil
}

type endpointTransport struct {
	origin  *url.URL
	headers map[string]string
}

func (t endpointTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL.User != nil || req.URL.Scheme != t.origin.Scheme || !strings.EqualFold(req.URL.Host, t.origin.Host) {
		return nil, errors.New("MCP endpoint origin changed")
	}
	req = req.Clone(req.Context())
	for k, v := range t.headers {
		if req.Header.Get(k) == "" {
			req.Header.Set(k, v)
		}
	}
	return http.DefaultTransport.RoundTrip(req)
}

func (c *remoteClient) ListTools(ctx context.Context) ([]Tool, error) {
	var entries []Tool
	for entry, err := range c.session.Tools(ctx, nil) {
		if err != nil {
			return nil, errors.New("MCP tool discovery failed")
		}
		if slices.Contains(c.spec.AllowedTools, entry.Name) && modelToolName.MatchString("mcp_"+c.Name()+"_"+entry.Name) {
			entries = append(entries, Tool{Name: entry.Name, Description: entry.Description, InputSchema: entry.InputSchema})
		}
	}
	if len(entries) == 0 {
		return nil, errors.New("no allowlisted MCP tools found")
	}
	return entries, nil
}
func (c *remoteClient) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if !slices.Contains(c.spec.AllowedTools, name) {
		return "", errors.New("MCP tool outside allowlist")
	}
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	result, err := c.session.CallTool(ctx, &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil || result.IsError {
		return "", errors.New("external MCP tool call failed")
	}
	var content any = result.StructuredContent
	if content == nil {
		var parts []string
		for _, item := range result.Content {
			if value, ok := item.(*sdk.TextContent); ok {
				parts = append(parts, value.Text)
			}
		}
		if len(parts) == 0 {
			return "", errors.New("unsupported MCP tool result")
		}
		content = parts
	}
	data, err := json.Marshal(content)
	if len(data) > 256<<10 {
		return "", errors.New("MCP result exceeds size limit")
	}
	return string(data), err
}

func RegisterRemote(ctx context.Context, reg *tools.Registry, spec config.MCPServerConfig, authorize ...func(context.Context) error) (func(), error) {
	if !modelToolName.MatchString(spec.Name) || len(spec.AllowedTools) == 0 {
		return nil, errors.New("MCP server name and allowlist required")
	}
	client := &remoteClient{spec: spec}
	connection, cancel := context.WithCancel(context.WithoutCancel(ctx))
	stop := context.AfterFunc(ctx, cancel)
	defer stop()
	if err := client.Connect(connection); err != nil {
		cancel()
		return nil, err
	}
	closeSession := func() { cancel(); _ = client.Close() }
	entries, err := client.ListTools(ctx)
	if err != nil {
		closeSession()
		return nil, err
	}
	var registered []string
	for _, entry := range entries {
		adapter := NewToolAdapter(client, entry)
		adapter.readOnly = slices.Contains(spec.ReadOnlyTools, entry.Name)
		if len(authorize) > 0 {
			adapter.authorize = authorize[0]
		}
		adapter.alias = "mcp_" + spec.Name + "_" + entry.Name
		if err := reg.Register(adapter); err != nil {
			for _, name := range registered {
				reg.Remove(name)
			}
			closeSession()
			return nil, err
		}
		registered = append(registered, adapter.Name())
	}
	return closeSession, nil
}
