package plugins

// referralLinks holds the Hopper developer's referral/affiliate link per
// hoster, keyed by plugin Name(). It is the single place to maintain them:
// paste the exact link from each hoster's affiliate dashboard. An empty
// value means "no referral link" — the UI then links the hoster's plain
// premium page (PremiumLinker.PremiumURL) instead. Users can always opt
// out with accounts.referral_links: false in hopper.yaml.
var referralLinks = map[string]string{
	"realdebrid": "", // e.g. https://real-debrid.com/?id=...
	"rapidgator": "",
	"onefichier": "",
	"ddownload":  "",
	"katfile":    "",
	"alldebrid":  "",
	"debridlink": "",
	"premiumize": "",
}

// ReferralLinkFor returns the configured referral link for plugin name,
// or "".
func ReferralLinkFor(name string) string { return referralLinks[name] }
