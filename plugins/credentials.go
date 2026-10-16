package plugins

// CredentialField describes one input of a hoster's account form. Name is
// the plugins.Account slot it fills: "username" or "password" (the only
// two the Web UI's form and POST /api/v1/accounts carry).
type CredentialField struct {
	Name        string `json:"name"`
	Label       string `json:"label"`
	Secret      bool   `json:"secret"`
	Required    bool   `json:"required"`
	Placeholder string `json:"placeholder,omitempty"`
	// Help is a short hint where to find the value, shown under the field.
	Help string `json:"help,omitempty"`
}

// CredentialDescriber is implemented by Credentialed plugins whose account
// isn't a plain username + password — an API token, two API keys, ... —
// so the Web UI can label its form correctly and the API only requires
// what the plugin actually reads.
type CredentialDescriber interface {
	CredentialFields() []CredentialField
}

// DefaultCredentialFields is the form for a Credentialed plugin that
// doesn't implement CredentialDescriber: username + password.
var DefaultCredentialFields = []CredentialField{
	{Name: "username", Label: "Username", Required: true},
	{Name: "password", Label: "Password", Secret: true, Required: true},
}

// CredentialFieldsFor returns h's account form fields.
func CredentialFieldsFor(h Hoster) []CredentialField {
	if d, ok := h.(CredentialDescriber); ok {
		return d.CredentialFields()
	}
	return DefaultCredentialFields
}

// TokenCredentialFields builds the account form for a hoster whose only
// credential is one API token/key: the token goes into Password (see
// TokenFromAccount), the username is only a display label.
func TokenCredentialFields(label, help string) []CredentialField {
	return []CredentialField{
		{Name: "username", Label: "Name", Placeholder: "optional label"},
		{Name: "password", Label: label, Secret: true, Required: true, Help: help},
	}
}
