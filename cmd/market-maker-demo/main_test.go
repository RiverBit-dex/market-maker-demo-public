package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/riverbit-dex/market-maker-demo-public/internal/hlexchange"
)

func TestReadAssetInfoFindsBTCFromInfoResponse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/info" {
			t.Fatalf("path = %s, want /info", r.URL.Path)
		}
		_, _ = w.Write([]byte(`[{"universe":[{"name":"RBT","szDecimals":2,"maxLeverage":20},{"name":"BTC","szDecimals":5,"maxLeverage":40}]},[]]`))
	}))
	defer server.Close()
	client, err := hlexchange.New(server.URL, "0000000000000000000000000000000000000000000000000000000000000001", false)
	if err != nil {
		t.Fatal(err)
	}
	index, decimals, leverage, err := readAssetInfo(context.Background(), client, "BTC")
	if err != nil {
		t.Fatal(err)
	}
	if index != 1 || decimals != 5 || leverage != 40 {
		t.Fatalf("got index=%d decimals=%d leverage=%d", index, decimals, leverage)
	}
}

func TestBatchModifyRejectsAssetMismatch(t *testing.T) {
	action := &hlexchange.BatchModify{Type: "batchModify", Modifies: []hlexchange.ModifyItem{{OID: 1, Order: hlexchange.Order{Asset: 1}}}}
	if err := validateActionAsset(action, 0); err == nil {
		t.Fatal("expected BTC asset mismatch to fail closed")
	}
}

func TestCancelByCloidRejectsAssetMismatch(t *testing.T) {
	var cloid hlexchange.Cloid
	if err := json.Unmarshal([]byte(`"0x1234567890abcdef1234567890abcdef"`), &cloid); err != nil {
		t.Fatal(err)
	}
	action := &hlexchange.CancelByCloid{Type: "cancelByCloid", Cancels: []hlexchange.CancelByCloidItem{{Asset: 1, Cloid: cloid}}}
	if err := validateActionAsset(action, 0); err == nil {
		t.Fatal("expected cancelByCloid asset mismatch to fail closed")
	}
	if err := validateActionAsset(action, 1); err != nil {
		t.Fatalf("matching asset should pass: %v", err)
	}
}
