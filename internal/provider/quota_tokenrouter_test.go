package provider

import (
	"testing"
)

func TestParseTokenRouterWallet(t *testing.T) {
	body := `{"data":{"topUpBalance":3.690708,"toppedUpSpent":36.309292,"voucherEfficientAmount":0,"voucherSpent":10},"message":"","success":true}`
	q := parseTokenRouterWallet(body)
	if q.Kind != QuotaMoney {
		t.Fatalf("kind = %q", q.Kind)
	}
	if q.Balance == nil || *q.Balance < 3.6906 || *q.Balance > 3.6908 {
		t.Fatalf("balance = %v, want ~3.6907", q.Balance)
	}
	if q.BalanceUnit != "USD" {
		t.Fatalf("unit = %q", q.BalanceUnit)
	}
	if q.CostTotal == nil || *q.CostTotal < 46.309 || *q.CostTotal > 46.310 {
		t.Fatalf("cost = %v, want ~46.309", q.CostTotal)
	}
}
