package awg

import (
	"encoding/json"
	"fmt"
)

// MarshalJSON writes a key in its .conf form, so a stored configuration can
// be read and compared by eye instead of being an array of 32 numbers.
func (k Key) MarshalJSON() ([]byte, error) {
	if k.IsZero() {
		return json.Marshal("")
	}
	return json.Marshal(k.Base64())
}

// UnmarshalJSON reads a key from its .conf form. An empty string means unset.
func (k *Key) UnmarshalJSON(raw []byte) error {
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		return fmt.Errorf("key must be a base64 string: %w", err)
	}
	if s == "" {
		*k = Key{}
		return nil
	}
	parsed, err := ParseKeyBase64(s)
	if err != nil {
		return err
	}
	*k = parsed
	return nil
}
