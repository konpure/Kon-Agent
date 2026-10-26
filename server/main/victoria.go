package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// postToVM writes samples to VictoriaMetrics' JSON-lines import API.
// Failures are reported to the caller for logging but never block the
// receive path — the monitoring side-channel must not stall ingestion.
func postToVM(ctx context.Context, vmAddr string, lines []vmSample) error {
	if len(lines) == 0 {
		return nil
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	for _, l := range lines {
		if err := enc.Encode(l); err != nil {
			return fmt.Errorf("failed to encode VM line: %w", err)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, vmAddr+"/api/v1/import", &buf)
	if err != nil {
		return fmt.Errorf("failed to build VM request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("failed to post to VM: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode > 299 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("VM import returned %s: %s", resp.Status, body)
	}
	return nil
}
