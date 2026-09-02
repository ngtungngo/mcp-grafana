//go:build unit

package tools

import (
	"context"
	"testing"

	"github.com/grafana/grafana-plugin-sdk-go/backend"
	"github.com/grafana/grafana-plugin-sdk-go/data"
	mcpgrafana "github.com/grafana/mcp-grafana"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// genericPanelDashboard builds a dashboard with a single panel backed by a
// datasource type that has no dedicated execution path (a Neo4j datasource
// here). The target exercises the concerns specific to the generic path:
//   - cypherQuery is a datasource-specific query field, so it must be recognised
//     as the panel query and interpolated;
//   - ${muid} is a dashboard variable, substituted before dispatch;
//   - nested and boolean fields must survive untouched, since the generic path
//     forwards the raw target rather than rebuilding it.
//
// refId is omitted so the default can be asserted.
func genericPanelDashboard(dsType string) map[string]interface{} {
	return map[string]interface{}{
		"templating": map[string]interface{}{
			"list": []interface{}{
				map[string]interface{}{
					"name":    "muid",
					"type":    "constant",
					"query":   "M123456",
					"current": map[string]interface{}{"value": "M123456"},
				},
			},
		},
		"panels": []interface{}{
			map[string]interface{}{
				"id":    float64(11),
				"title": "Identity graph",
				"type":  "table",
				"datasource": map[string]interface{}{
					"uid":  "neo4j-uid",
					"type": dsType,
				},
				"targets": []interface{}{
					map[string]interface{}{
						"cypherQuery": "MATCH (m:MUID {id: '${muid}'}) RETURN m",
						"format":      "table",
						"legacyMode":  false,
						"options": map[string]interface{}{
							"label": "muid=${muid}",
						},
					},
				},
			},
		},
	}
}

// TestRunSinglePanelQuery_GenericDatasourceDispatch drives the full panel-query
// path for a datasource with no dedicated handler: dispatch to the generic path,
// query extraction from a datasource-specific field, variable substitution
// (including nested target fields), request construction and result passthrough.
func TestRunSinglePanelQuery_GenericDatasourceDispatch(t *testing.T) {
	frames := data.Frames{
		data.NewFrame("", data.NewField("m", nil, []string{"M123456"})),
	}
	var captured capturedQuery
	ts := newDSQueryTestServer(t, &captured, frames)
	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{URL: ts.URL})

	result, err := runSinglePanelQuery(ctx, singlePanelQueryParams{
		DB:         genericPanelDashboard("neo4j-datasource"),
		PanelID:    11,
		QueryIndex: 0,
		Start:      "now-1h",
		End:        "now",
	})
	require.NoError(t, err)

	// Dispatch: the unknown type reached the generic path instead of erroring out.
	assert.Equal(t, "neo4j-datasource", result.DatasourceType)
	assert.Equal(t, "neo4j-uid", result.DatasourceUID)

	// Query extraction and substitution: cypherQuery was picked up as the panel
	// query and the dashboard variable was resolved.
	assert.Equal(t, "MATCH (m:MUID {id: 'M123456'}) RETURN m", result.Query)

	// Result passthrough: the generic path returns the raw per-refId responses.
	responses, ok := result.Results.(backend.Responses)
	require.True(t, ok, "expected backend.Responses, got %T", result.Results)
	require.Contains(t, responses, "A")
	require.Len(t, responses["A"].Frames, 1)

	// Request construction: the datasource-specific query field is forwarded with
	// variables substituted, including inside nested fields.
	assert.Equal(t, "MATCH (m:MUID {id: 'M123456'}) RETURN m", captured.query["cypherQuery"])
	options, ok := captured.query["options"].(map[string]interface{})
	require.True(t, ok, "nested target fields should be preserved")
	assert.Equal(t, "muid=M123456", options["label"])

	// Target preservation: unrelated fields survive untouched.
	assert.Equal(t, "table", captured.query["format"])
	assert.Equal(t, false, captured.query["legacyMode"])

	// A refId is required by /api/ds/query, so it is defaulted when the panel omits it.
	assert.Equal(t, "A", captured.query["refId"])

	ds, ok := captured.query["datasource"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "neo4j-uid", ds["uid"])
	assert.Equal(t, "neo4j-datasource", ds["type"])

	// The envelope carries the resolved time range.
	assert.NotEmpty(t, captured.payload["from"])
	assert.NotEmpty(t, captured.payload["to"])
}

// TestRunSinglePanelQuery_GenericDatasourceKeepsRefID verifies that a refId set
// by the panel is not overwritten by the default.
func TestRunSinglePanelQuery_GenericDatasourceKeepsRefID(t *testing.T) {
	db := genericPanelDashboard("neo4j-datasource")
	panels := db["panels"].([]interface{})
	targets := panels[0].(map[string]interface{})["targets"].([]interface{})
	targets[0].(map[string]interface{})["refId"] = "B"

	var captured capturedQuery
	ts := newDSQueryTestServer(t, &captured, data.Frames{
		data.NewFrame("", data.NewField("m", nil, []string{"M123456"})),
	})
	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{URL: ts.URL})

	_, err := runSinglePanelQuery(ctx, singlePanelQueryParams{
		DB:         db,
		PanelID:    11,
		QueryIndex: 0,
		Start:      "now-1h",
		End:        "now",
	})
	require.NoError(t, err)
	assert.Equal(t, "B", captured.query["refId"])
}

// TestRunSinglePanelQuery_GenericDatasourceVariableOverride verifies that an
// explicit variable override wins over the dashboard's own value on the generic
// path too.
func TestRunSinglePanelQuery_GenericDatasourceVariableOverride(t *testing.T) {
	var captured capturedQuery
	ts := newDSQueryTestServer(t, &captured, data.Frames{
		data.NewFrame("", data.NewField("m", nil, []string{"M999999"})),
	})
	ctx := mcpgrafana.WithGrafanaConfig(context.Background(), mcpgrafana.GrafanaConfig{URL: ts.URL})

	result, err := runSinglePanelQuery(ctx, singlePanelQueryParams{
		DB:         genericPanelDashboard("neo4j-datasource"),
		PanelID:    11,
		QueryIndex: 0,
		Start:      "now-1h",
		End:        "now",
		Variables:  map[string]string{"muid": "M999999"},
	})
	require.NoError(t, err)

	assert.Equal(t, "MATCH (m:MUID {id: 'M999999'}) RETURN m", result.Query)
	assert.Equal(t, "MATCH (m:MUID {id: 'M999999'}) RETURN m", captured.query["cypherQuery"])
}

func TestExecuteGenericPanelQuery_MissingTarget(t *testing.T) {
	tests := []struct {
		name      string
		panelData *panelInfo
	}{
		{name: "nil panel", panelData: nil},
		{name: "nil raw target", panelData: &panelInfo{ID: 11}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := executeGenericPanelQuery(context.Background(), "neo4j-datasource", "neo4j-uid", tt.panelData, "now-1h", "now", nil)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "panel target not available")
		})
	}
}

func TestExecuteGenericPanelQuery_InvalidTimeRange(t *testing.T) {
	panelData := &panelInfo{ID: 11, RawTarget: map[string]interface{}{"cypherQuery": "RETURN 1"}}

	_, err := executeGenericPanelQuery(context.Background(), "neo4j-datasource", "neo4j-uid", panelData, "not-a-time", "now", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing start time")

	_, err = executeGenericPanelQuery(context.Background(), "neo4j-datasource", "neo4j-uid", panelData, "now-1h", "not-a-time", nil)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "parsing end time")
}

// TestExtractQueryExpression_CypherQuery covers the new query field, including
// that it does not take precedence over the established ones.
func TestExtractQueryExpression_CypherQuery(t *testing.T) {
	assert.Equal(t, "MATCH (n) RETURN n", extractQueryExpression(map[string]interface{}{
		"cypherQuery": "MATCH (n) RETURN n",
	}))

	assert.Equal(t, "up", extractQueryExpression(map[string]interface{}{
		"expr":        "up",
		"cypherQuery": "MATCH (n) RETURN n",
	}))
}
