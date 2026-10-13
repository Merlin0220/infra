package keys

// Client-facing messages for a rejected API key. Every service that answers
// for an API key uses these, so a client sees the same text wherever the key
// is rejected.
const (
	// InvalidAPIKeyMessage leads every rejection of a well-prefixed key.
	InvalidAPIKeyMessage = "Invalid API key, please visit https://docs.e2b.dev/api-key for more information."
	// MalformedAPIKeyMessage answers a key without the ApiKeyPrefix.
	MalformedAPIKeyMessage = `API key is malformed: expected the "` + ApiKeyPrefix + `" prefix, visit https://docs.e2b.dev/api-key for more information`

	// InvalidAPIKeyFormat is the detail for a key that is not hex after the prefix.
	InvalidAPIKeyFormat = "Invalid API key format"
	// UnknownAPIKey is the detail for a well-formed key that no team owns.
	UnknownAPIKey = "Cannot get the team for the given API key"
)

// InvalidAPIKey is the full message for a rejected key: InvalidAPIKeyMessage,
// then detail on its own line.
func InvalidAPIKey(detail string) string {
	return InvalidAPIKeyMessage + "\n" + detail
}
