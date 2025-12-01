package mcp_test

import (
	"testing"

	"github.com/alecthomas/assert/v2"
	mcp "github.com/grafana/xk6-mcp"
	"go.k6.io/k6/js/modulestest"
)

func setupRuntime(t *testing.T) *modulestest.VU {
	t.Helper()

	rt := modulestest.NewRuntime(t)
	vu := rt.VU

	mod, ok := mcp.New().NewModuleInstance(vu).(*mcp.MCPInstance)
	assert.True(t, ok)
	assert.NoError(t, vu.RuntimeField.Set("mcp", mod.Exports().Named))

	return vu
}

func TestInit(t *testing.T) {
	rt := modulestest.NewRuntime(t)

	_, ok := mcp.New().NewModuleInstance(rt.VU).(*mcp.MCPInstance)

	assert.True(t, ok)
}
