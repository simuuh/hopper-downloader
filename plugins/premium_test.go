package plugins

import "testing"

type linker struct {
	fakeHoster
	premium, referral string
}

func (l linker) PremiumURL() string  { return l.premium }
func (l linker) ReferralURL() string { return l.referral }

type fakeHoster struct{}

func (fakeHoster) Match(string) bool                                 { return false }
func (fakeHoster) Resolve(string, *Account) (*FileInfo, error)       { return nil, nil }
func (fakeHoster) FetchDownloadURL(string, *Account) (string, error) { return "", nil }
func (fakeHoster) Name() string                                      { return "fake" }
func (fakeHoster) Domains() []string                                 { return nil }

func TestPremiumLink(t *testing.T) {
	withRef := linker{premium: "https://h/premium", referral: "https://h/?ref=dev"}
	noRef := linker{premium: "https://h/premium"}

	if l, ref := PremiumLink(withRef, true); l != "https://h/?ref=dev" || !ref {
		t.Errorf("referral enabled: %q %v", l, ref)
	}
	if l, ref := PremiumLink(withRef, false); l != "https://h/premium" || ref {
		t.Errorf("referral disabled by user: %q %v", l, ref)
	}
	if l, ref := PremiumLink(noRef, true); l != "https://h/premium" || ref {
		t.Errorf("no referral configured: %q %v", l, ref)
	}
	if l, _ := PremiumLink(fakeHoster{}, true); l != "" {
		t.Errorf("non-linker: %q", l)
	}
}

func TestReferralLinks_KeysMatchCredentialedPlugins(t *testing.T) {
	// A typo in a referral.go key would silently never be used.
	for _, k := range []string{"realdebrid", "rapidgator", "onefichier", "ddownload", "katfile"} {
		if _, ok := referralLinks[k]; !ok {
			t.Errorf("referralLinks has no entry for %q", k)
		}
	}
}
