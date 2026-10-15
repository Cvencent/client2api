package trae

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTraeBillingReadsDisplayContactConfigString(t *testing.T) {
	raw := []byte(`{
		"config_name": "demo",
		"display_contact_config": "{\"consumption_rate\":{\"enable\":true,\"data\":{\"rate\":0.8}},\"activity_discount\":{\"enable\":true,\"data\":{\"current\":{\"discount_type\":\"limited\",\"consumption_rate\":0.08,\"before_consumption_rate\":0.8,\"end_at\":2000000000}}}}"
	}`)
	var entry modelEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("unmarshal model entry: %v", err)
	}

	base, current, label, ok := entry.traeBilling(time.Unix(1000000000, 0))
	if !ok {
		t.Fatal("a valid display_contact_config must produce a billing rate")
	}
	if base != 0.8 || current != 0.08 {
		t.Fatalf("billing = (%v, %v), want (0.8, 0.08)", base, current)
	}
	if label == "" {
		t.Fatal("an active limited discount must carry a label")
	}
}

func TestTraeBillingReadsFeaturesAndAllowsFreeRate(t *testing.T) {
	raw := []byte(`{
		"config_name": "free",
		"features": {
			"consumption_rate": {"enable": true, "data": {"rate": 0}},
			"activity_discount": {"enable": true, "data": {"current": {"discount_type": "none"}}}
		}
	}`)
	var entry modelEntry
	if err := json.Unmarshal(raw, &entry); err != nil {
		t.Fatalf("unmarshal model entry: %v", err)
	}

	base, current, label, ok := entry.traeBilling(time.Unix(1000000000, 0))
	if !ok || base != 0 || current != 0 {
		t.Fatalf("free billing = (%v, %v, %q, %v), want (0, 0, \"\", true)", base, current, label, ok)
	}
	if label != "" {
		t.Fatalf("discount_type=none must not produce a promotion label, got %q", label)
	}
}

func TestTraeBillingIgnoresInactivePromotions(t *testing.T) {
	now := time.Unix(1000000000, 0)
	cases := map[string]string{
		"discount disabled": `{"consumption_rate":{"enable":true,"data":{"rate":0.5}},"activity_discount":{"enable":false,"data":{"current":{"discount_type":"limited","consumption_rate":0.1,"before_consumption_rate":0.5}}}}`,
		"no discount":       `{"consumption_rate":{"enable":true,"data":{"rate":0.5}},"activity_discount":{"enable":true,"data":{"current":{"discount_type":"none","consumption_rate":0.5,"before_consumption_rate":0.5}}}}`,
		"expired":           `{"consumption_rate":{"enable":true,"data":{"rate":0.5}},"activity_discount":{"enable":true,"data":{"current":{"discount_type":"limited","consumption_rate":0.1,"before_consumption_rate":0.5,"end_at":999999999}}}}`,
		"not a reduction":   `{"consumption_rate":{"enable":true,"data":{"rate":0.5}},"activity_discount":{"enable":true,"data":{"current":{"discount_type":"limited","consumption_rate":0.6,"before_consumption_rate":0.5}}}}`,
	}
	for name, cfg := range cases {
		t.Run(name, func(t *testing.T) {
			rawEntry, err := json.Marshal(map[string]any{"display_contact_config": cfg})
			if err != nil {
				t.Fatalf("marshal model entry: %v", err)
			}
			var entry modelEntry
			if err := json.Unmarshal(rawEntry, &entry); err != nil {
				t.Fatalf("unmarshal model entry: %v", err)
			}
			base, current, label, ok := entry.traeBilling(now)
			if !ok || base != 0.5 || current != 0.5 || label != "" {
				t.Fatalf("billing = (%v, %v, %q, %v), want (0.5, 0.5, \"\", true)", base, current, label, ok)
			}
		})
	}
}
