package queue

import (
	"testing"

	"github.com/simuuh/hopper-downloader/plugins"
)

func names(hs []plugins.Hoster) []string {
	out := make([]string, len(hs))
	for i, h := range hs {
		out[i] = h.Name()
	}
	return out
}

func TestRankHosters_ListedFirstThenReverseRegistration(t *testing.T) {
	all := []plugins.Hoster{
		&fakeHoster{name: "directhttp"},
		&fakeHoster{name: "rapidgator"},
		&fakeHoster{name: "premiumize"},
		&fakeHoster{name: "alldebrid"},
		&fakeHoster{name: "realdebrid"},
	}
	got := names(RankHosters(all, []string{"premiumize", "unknown", "alldebrid", "premiumize"}))
	want := []string{"premiumize", "alldebrid", "realdebrid", "rapidgator", "directhttp"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	if def := names(RankHosters(all, nil)); def[0] != "realdebrid" || def[4] != "directhttp" {
		t.Errorf("default order = %v, want last registered first", def)
	}
}

func TestMatchHoster_AccountPriorityPicksPreferredMultiHoster(t *testing.T) {
	rd := &fakeHoster{name: "realdebrid", matchHosts: []string{"rapidgator.net"}}
	ad := &fakeHoster{name: "alldebrid", matchHosts: []string{"rapidgator.net"}}
	native := &fakeHoster{name: "rapidgator", matchHosts: []string{"rapidgator.net"}}
	all := []plugins.Hoster{native, ad, rd}
	accounts := &fakeAccountLookup{withAccount: map[string]bool{"realdebrid": true, "alldebrid": true, "rapidgator": true}}
	url := "https://rapidgator.net/file/abc"

	if h, _ := MatchHoster(all, url, accounts, nil); h.Name() != "realdebrid" {
		t.Errorf("default: matched %s, want realdebrid (registered last)", h.Name())
	}
	if h, _ := MatchHoster(all, url, accounts, []string{"alldebrid"}); h.Name() != "alldebrid" {
		t.Errorf("priority [alldebrid]: matched %s", h.Name())
	}
	if h, _ := MatchHoster(all, url, accounts, []string{"rapidgator", "realdebrid"}); h.Name() != "rapidgator" {
		t.Errorf("priority [rapidgator ...]: matched %s", h.Name())
	}

	// A preferred plugin without an account is skipped.
	accounts.withAccount["alldebrid"] = false
	if h, _ := MatchHoster(all, url, accounts, []string{"alldebrid", "rapidgator"}); h.Name() != "rapidgator" {
		t.Errorf("preferred without account: matched %s, want rapidgator", h.Name())
	}
}
