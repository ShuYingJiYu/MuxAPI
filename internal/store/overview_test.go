package store

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/mirainya/muxapi/internal/upstream"
)

func TestOverviewTrendsScopesBalancesAndSuccess(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "overview.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	gid, err := st.CreateGroup("primary", "")
	if err != nil {
		t.Fatal(err)
	}
	first := &upstream.Upstream{Name: "first", BaseURL: "http://first", APIKey: "k1", BillingType: upstream.BillingSub2API, Enabled: true}
	second := &upstream.Upstream{Name: "second", BaseURL: "http://second", APIKey: "k2", BillingType: upstream.BillingSub2API, Enabled: true}
	if err := st.Create(first); err != nil {
		t.Fatal(err)
	}
	if err := st.Create(second); err != nil {
		t.Fatal(err)
	}
	if err := st.AddMember(gid, first.ID, 1, 1); err != nil {
		t.Fatal(err)
	}
	tagID, err := st.CreateTag("codex", "blue")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.BatchUpdateUpstreams([]int64{first.ID}, UpstreamBatchUpdate{PrimaryTagID: &tagID}); err != nil {
		t.Fatal(err)
	}

	end := time.Now().Truncate(time.Hour)
	remaining := 8.0
	if err := st.SaveBillingSuccess(BillingStatus{UpstreamID: first.ID, Currency: "USD", Remaining: &remaining, Status: "ok", ObservedAt: end.Add(-time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}
	remaining = 10
	if err := st.SaveBillingSuccess(BillingStatus{UpstreamID: first.ID, Currency: "USD", Remaining: &remaining, Status: "ok", ObservedAt: end.Add(-2 * time.Hour).Unix()}); err != nil {
		t.Fatal(err)
	}

	trend, err := st.OverviewTrends(gid, LookupOverviewTrendWindow("24h"), end.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if trend.UpstreamCount != 1 || len(trend.Balances) != 1 || len(trend.Balances[0].Points) != 25 || len(trend.UpstreamBalances) != 1 {
		t.Fatalf("unexpected scoped trend: %+v", trend)
	}
	if trend.UpstreamBalances[0].Name != "first" || len(trend.UpstreamBalances[0].Points) != 25 {
		t.Fatalf("unexpected upstream balance series: %+v", trend.UpstreamBalances[0])
	}
	last := trend.Balances[0].Points[len(trend.Balances[0].Points)-1]
	if last.Remaining == nil || *last.Remaining != 8 {
		t.Fatalf("latest balance should be 8, got %+v", last.Remaining)
	}
	if len(trend.Success) != 25 || trend.Success[len(trend.Success)-1].Rate != nil {
		t.Fatalf("unexpected empty success trend: %+v", trend.Success[len(trend.Success)-1])
	}
	tagTrend, err := st.OverviewTrendsByTag(tagID, LookupOverviewTrendWindow("24h"), end.Add(30*time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if tagTrend.TagID != tagID || tagTrend.UpstreamCount != 1 || len(tagTrend.Balances) != 1 {
		t.Fatalf("unexpected tag trend: %+v", tagTrend)
	}
}
