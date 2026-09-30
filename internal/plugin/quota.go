package plugin

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"net/http"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type quotaUpstreamWindow struct {
	Percent  *float64 `json:"percent"`
	ResetsAt string   `json:"resetsAt"`
}

// FetchQuota uses the credential selected by the host, never a default config key.
func (m *Manager) FetchQuota(ctx context.Context, req pluginapi.QuotaFetchRequest) (pluginapi.QuotaFetchResponse, error) {
	empty := pluginapi.QuotaFetchResponse{}
	if req.Provider != ProviderID {
		return empty, fmt.Errorf("unsupported quota provider")
	}
	key := strings.TrimSpace(req.Attributes["api_key"])
	if key == "" {
		return empty, fmt.Errorf("quota credential has no API key")
	}
	m.mu.RLock()
	baseURL, timeout := m.cfg.BaseURL, m.cfg.RequestTimeout
	m.mu.RUnlock()
	if m.bridge == nil {
		return empty, fmt.Errorf("quota bridge unavailable")
	}
	if timeout <= 0 || timeout > 20*time.Second {
		timeout = 20 * time.Second
	}
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	resp, err := m.bridge.Do(ctx, pluginapi.HTTPRequest{Method: http.MethodGet, URL: strings.TrimRight(baseURL, "/") + "/usage", Headers: http.Header{"Authorization": {"Bearer " + key}, "Accept": {"application/json"}}})
	if err != nil || resp.StatusCode != http.StatusOK {
		return empty, fmt.Errorf("quota upstream request failed")
	}
	return normalizeQuota(resp.Body)
}

func normalizeQuota(body []byte) (pluginapi.QuotaFetchResponse, error) {
	var decoded struct {
		Usage map[string]quotaUpstreamWindow `json:"usage"`
	}
	if json.Unmarshal(body, &decoded) != nil {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota response invalid")
	}
	buckets := []pluginapi.QuotaBucket{}
	for _, name := range []string{"rolling", "weekly", "monthly"} {
		window, exists := decoded.Usage[name]
		if !exists || window.Percent == nil {
			continue
		}
		percent := *window.Percent
		if math.IsNaN(percent) || math.IsInf(percent, 0) || percent < 0 || percent > 100 {
			return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota percentage invalid")
		}
		reset := ""
		if window.ResetsAt != "" {
			parsed, err := time.Parse(time.RFC3339Nano, window.ResetsAt)
			if err != nil {
				return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota reset time invalid")
			}
			reset = parsed.UTC().Format(time.RFC3339Nano)
		}
		buckets = append(buckets, pluginapi.QuotaBucket{Window: name, RemainingFraction: 1 - percent/100, ResetTime: reset})
	}
	if len(buckets) == 0 {
		return pluginapi.QuotaFetchResponse{}, fmt.Errorf("quota response has no readings")
	}
	return pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: "Go"}, Groups: []pluginapi.QuotaGroup{{DisplayName: "OpenCode Go", Buckets: buckets}}}, nil
}
