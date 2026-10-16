package plugins

// PremiumLinker is implemented by hoster plugins that can point the user
// at the hoster's own page to buy or extend a premium account — shown in
// the Web UI's Accounts page ("Buy / extend premium") and next to an
// account that's about to expire.
//
// ReferralURL is the Hopper developer's affiliate/referral link for that
// page, if one is configured (plugins/referral.go — empty by default).
// Buying through it supports Hopper's development at no extra cost to the
// user. PremiumURL is the plain page without any referral, used when no
// referral link is configured or the user disabled referral links
// (accounts.referral_links: false in hopper.yaml).
type PremiumLinker interface {
	PremiumURL() string
	ReferralURL() string
}

// PremiumLink returns the link to show for h: its referral link when
// useReferral is set and one is configured, else its plain premium page,
// else "" if h isn't a PremiumLinker at all. isReferral reports which one
// was returned so the UI can say so — no hidden affiliate links.
func PremiumLink(h Hoster, useReferral bool) (link string, isReferral bool) {
	pl, ok := h.(PremiumLinker)
	if !ok {
		return "", false
	}
	if useReferral {
		if r := pl.ReferralURL(); r != "" {
			return r, true
		}
	}
	return pl.PremiumURL(), false
}
