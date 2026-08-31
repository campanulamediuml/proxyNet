package singbox

import (
	"encoding/json"
	"testing"
)

func TestGenerateClientConfig(t *testing.T) {
	cfg := GenerateClientConfig("tun0", "172.19.0.1/30", 1400, 1080, []string{"192.168.3.33"}, "127.0.0.1", []string{"vEthernet (WSL)"}, []string{"192.168.0.0/16"})

	var v map[string]interface{}
	if err := json.Unmarshal([]byte(cfg), &v); err != nil {
		t.Fatalf("config is not valid JSON: %v", err)
	}

	if v["log"] == nil {
		t.Error("missing log section")
	}
	if v["dns"] == nil {
		t.Error("missing dns section")
	}
	if v["inbounds"] == nil {
		t.Error("missing inbounds section")
	}
	if v["outbounds"] == nil {
		t.Error("missing outbounds section")
	}
	if v["route"] == nil {
		t.Error("missing route section")
	}
}
