package proxy_test

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/belune-hq/belune/internal/proxy"
)

func TestParseFeatureConfig_RateLimit(t *testing.T) {
	ok := map[string]string{
		"basic":   `{"window":"1s","max_events":10}`,
		"minutes": `{"window":"1m","max_events":1}`,
		"ceiling": `{"window":"1h","max_events":1000}`,
	}
	for name, raw := range ok {
		t.Run("accepts "+name, func(t *testing.T) {
			parsed, err := proxy.ParseFeatureConfig(proxy.FeatureRateLimit, json.RawMessage(raw))
			require.NoError(t, err)
			assert.IsType(t, &proxy.RateLimitConfig{}, parsed)
		})
	}

	bad := map[string]string{
		// 0 answers nothing at all on Caddy, and is what an omitted field becomes.
		"max_events omitted":  `{"window":"1s"}`,
		"max_events zero":     `{"window":"1s","max_events":0}`,
		"max_events negative": `{"window":"1s","max_events":-1}`,
		"window omitted":      `{"max_events":10}`,
		"window unparseable":  `{"window":"soon","max_events":10}`,
		"window below floor":  `{"window":"500ms","max_events":10}`,
		"window above cap":    `{"window":"2h","max_events":10}`,
		// Pre-0.1.3 rows must be refused, not translated.
		"legacy rate/burst": `{"rate":"10/s","burst":5}`,
		// Zone and key are not the operator's to set.
		"zone supplied": `{"window":"1s","max_events":10,"zone":"shared"}`,
		"key supplied":  `{"window":"1s","max_events":10,"key":"{http.request.header.X-Foo}"}`,
	}
	for name, raw := range bad {
		t.Run("rejects "+name, func(t *testing.T) {
			_, err := proxy.ParseFeatureConfig(proxy.FeatureRateLimit, json.RawMessage(raw))
			require.Error(t, err)
		})
	}

	t.Run("legacy rejection names the new shape", func(t *testing.T) {
		_, err := proxy.ParseFeatureConfig(proxy.FeatureRateLimit, json.RawMessage(`{"rate":"10/s"}`))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "max_events")
		assert.Contains(t, err.Error(), "window")
	})
}

// The rejection must be targeted: the features the dashboard actually offers
// still have to parse.
func TestParseFeatureConfig_OfferedFeaturesStillParse(t *testing.T) {
	cases := map[string]string{
		proxy.FeatureBasicAuth:   `{"username":"u","hashed_password":"$2a$10$abcdefghijklmnopqrstuv"}`,
		proxy.FeatureHeaders:     `{"response":{"set":{"X-Frame-Options":["DENY"]}}}`,
		proxy.FeatureIPAllowlist: `{"ranges":["10.0.0.0/8"]}`,
		proxy.FeatureRedirect:    `{"from":"/old","to":"/new"}`,
	}
	for featureType, raw := range cases {
		t.Run(featureType, func(t *testing.T) {
			parsed, err := proxy.ParseFeatureConfig(featureType, json.RawMessage(raw))
			require.NoError(t, err)
			assert.NotNil(t, parsed)
		})
	}
}
