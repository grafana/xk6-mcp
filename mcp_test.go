package mcp_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/jsonschema-go/jsonschema"
	mcp "github.com/grafana/xk6-mcp"
	mcpsdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.k6.io/k6/js/modulestest"
)

type (
	MyToolInput struct {
		Id int `json:"id"`
	}

	MyToolOutput struct {
		Output string `json:"output"`
	}
)

const (
	toolName string = "myTool"
)

func setupRuntime(t *testing.T) *modulestest.VU {
	t.Helper()

	rt := modulestest.NewRuntime(t)
	vu := rt.VU

	mod, ok := mcp.New().NewModuleInstance(vu).(*mcp.MCPInstance)
	require.True(t, ok)
	require.NoError(t, vu.RuntimeField.Set("mcp", mod.Exports().Named))

	return vu
}

func streamableHandler(t *testing.T) (*mcpsdk.StreamableHTTPHandler, error) {
	t.Helper()

	inputSchema, err := jsonschema.For[MyToolInput](nil)
	if err != nil {
		return nil, err
	}
	toolHandler := func(context.Context, *mcpsdk.CallToolRequest, MyToolInput) (*mcpsdk.CallToolResult, any, error) {
		return nil, MyToolOutput{toolName}, nil
	}

	server := mcpsdk.NewServer(&mcpsdk.Implementation{Name: "test", Version: "1.0.0"}, nil)
	mcpsdk.AddTool(server, &mcpsdk.Tool{Name: toolName, InputSchema: inputSchema}, toolHandler)
	return mcpsdk.NewStreamableHTTPHandler(func(*http.Request) *mcpsdk.Server {
		return server
	},
		&mcpsdk.StreamableHTTPOptions{
			Stateless: true,
		},
	), nil
}

func TestInit(t *testing.T) {
	rt := modulestest.NewRuntime(t)

	_, ok := mcp.New().NewModuleInstance(rt.VU).(*mcp.MCPInstance)

	assert.True(t, ok)
}

func TestStreamableBearerAuth(t *testing.T) {
	jwtToken := "myjwt"
	var observedToken string
	handler, err := streamableHandler(t)

	require.NoError(t, err)
	handlerFunc := func(w http.ResponseWriter, r *http.Request) {
		authHeader := r.Header["Authorization"]
		if len(authHeader) > 0 {
			token, found := strings.CutPrefix(authHeader[0], "Bearer ")

			if found {
				observedToken = token
			}
		}
		handler.ServeHTTP(w, r)
	}

	ts := httptest.NewServer(http.HandlerFunc(handlerFunc))
	defer ts.Close()

	vu := setupRuntime(t)

	_, err = vu.RuntimeField.RunString(
		fmt.Sprintf(`const client = mcp.StreamableHTTPClient({
      base_url: "%s",
      auth: {
        bearer_token: "%s"
      }
    });`, ts.URL, jwtToken),
	)

	assert.NoError(t, err)
	assert.Equal(t, jwtToken, observedToken)
}
