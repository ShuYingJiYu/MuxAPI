package billing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/mirainya/muxapi/internal/store"
	"github.com/mirainya/muxapi/internal/upstream"
)

func TestManagerRefreshPersistsBillingStatus(t *testing.T) {
	provider := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/usage":
			w.Write([]byte(`{"remaining":12.5,"unit":"USD","usage":{"total":{"cost":20,"actual_cost":3}}}`))
		case "/v1/sub2api/billing":
			w.Write([]byte(`{"object":"sub2api.key_billing","group_rate_multiplier":0.15,"effective_rate_multiplier":0.15}`))
		default:
			http.NotFound(w, r)
		}
	}))
	defer provider.Close()

	st, err := store.Open(filepath.Join(t.TempDir(), "manager.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u := &upstream.Upstream{
		Name: "provider", BaseURL: provider.URL, APIKey: "sk-test",
		BillingType: upstream.BillingSub2API, Enabled: true,
	}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}

	state, err := NewManager(st).Refresh(context.Background(), u.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.Status != "ok" || state.Remaining == nil || *state.Remaining != 12.5 ||
		state.EffectiveMultiplier == nil || *state.EffectiveMultiplier != 0.15 {
		t.Fatalf("unexpected billing state: %+v", state)
	}
	snapshots, err := st.ListBillingSnapshots(u.ID, 10)
	if err != nil || len(snapshots) != 1 {
		t.Fatalf("unexpected billing snapshots: %+v, err=%v", snapshots, err)
	}
}

func TestManagerRefreshRejectsDisabledBilling(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "disabled.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	u := &upstream.Upstream{Name: "generic", BaseURL: "https://example.com", Enabled: true}
	if err := st.Create(u); err != nil {
		t.Fatal(err)
	}

	_, err = NewManager(st).Refresh(context.Background(), u.ID)
	if !errors.Is(err, ErrBillingDisabled) {
		t.Fatalf("disabled billing error = %v", err)
	}
}
