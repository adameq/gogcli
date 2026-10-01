package cmd

import (
	"context"

	"github.com/openclaw/gogcli/internal/outfmt"
)

type pageFetchFunc[T any] func(pageToken string) ([]T, string, error)

func loadPagedItems[T any](page string, all bool, fetch pageFetchFunc[T]) ([]T, string, error) {
	if all {
		items, err := collectAllPages(page, fetch)
		if err != nil {
			var zero []T
			return zero, "", err
		}
		return items, "", nil
	}
	return fetch(page)
}

// addPagedAggregates preserves command-specific metadata and counts result rows,
// excluding auxiliary arrays such as warnings and locations.
func addPagedAggregates(payload map[string]any, count int) {
	if _, exists := payload["count"]; !exists {
		payload["count"] = count
	}
	if _, exists := payload["has_more"]; !exists {
		tok, _ := payload["nextPageToken"].(string)
		payload["has_more"] = tok != ""
	}
}

func writePagedJSONResult(ctx context.Context, payload map[string]any, emptyCount int, failEmpty bool) error {
	addPagedAggregates(payload, emptyCount)
	if err := outfmt.WriteJSON(ctx, stdoutWriter(ctx), payload); err != nil {
		return err
	}
	if emptyCount == 0 {
		return failEmptyExit(failEmpty)
	}
	return nil
}
