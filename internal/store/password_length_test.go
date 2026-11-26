package store

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// bcrypt refuses more than 72 bytes. That used to surface as a 500 at setup
// (or a failed start from EZLLM_DASHBOARD_PASSWORD); now it is a named error.
func TestDashboardPasswordTooLong(t *testing.T) {
	d := openTest(t)
	long := strings.Repeat("a", 73)
	if err := d.CompleteDashboardSetup(context.Background(), long); !errors.Is(err, ErrDashboardPasswordTooLong) {
		t.Fatalf("setup with 73 bytes: %v, want ErrDashboardPasswordTooLong", err)
	}
	if err := d.CompleteDashboardSetup(context.Background(), strings.Repeat("a", 72)); err != nil {
		t.Fatalf("setup with 72 bytes: %v", err)
	}
	if err := d.ChangeDashboardPassword(context.Background(), strings.Repeat("a", 72), long); !errors.Is(err, ErrDashboardPasswordTooLong) {
		t.Fatalf("change to 73 bytes: %v, want ErrDashboardPasswordTooLong", err)
	}
}
