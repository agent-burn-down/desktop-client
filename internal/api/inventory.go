package api

import (
	"context"
	"fmt"
)

const MaxInventoryItems = 2_000

// InventoryItem is the strict display-safe allowlist accepted by the hosted
// replace-only inventory endpoint. Optional fields are omitted when unknown;
// raw paths, configuration, environment values, prompts, and schemas are never
// represented by this type.
type InventoryItem struct {
	Kind         string  `json:"kind"`
	Name         string  `json:"name"`
	Source       string  `json:"source"`
	Description  *string `json:"description,omitempty"`
	Version      *string `json:"version,omitempty"`
	ScriptCount  *int64  `json:"script_count,omitempty"`
	ToolCount    *int64  `json:"tool_count,omitempty"`
	SchemaTokens *int64  `json:"schema_tokens,omitempty"`
	Present      *bool   `json:"present,omitempty"`
}

type InventoryOut struct {
	Accepted   int    `json:"accepted"`
	Replaced   bool   `json:"replaced"`
	ObservedAt string `json:"observed_at"`
}

// SendInventory replaces this collector's hosted sanitized inventory.
func (c *Client) SendInventory(
	ctx context.Context, collectorID int64, observedAt string, items []InventoryItem,
) (*InventoryOut, error) {
	if len(items) > MaxInventoryItems {
		return nil, fmt.Errorf(
			"inventory of %d items exceeds max %d", len(items), MaxInventoryItems)
	}
	body := map[string]any{
		"collector_id": collectorID,
		"observed_at":  observedAt,
		"items":        items,
	}
	var out InventoryOut
	if err := c.postJSON(ctx, "/ingest/v1/inventory", body, &out); err != nil {
		return nil, err
	}
	return &out, nil
}
