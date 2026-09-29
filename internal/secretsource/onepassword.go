package secretsource

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
)

// onePasswordConfig is the JSON a onepassword source stores: which 1Password Connect server to read,
// the token that authorizes it, and the vault, item, and field to fetch. Connect is 1Password's
// self-hosted REST API, so a source resolves over HTTP with no op CLI on the runner. The vault and
// item may each be a Connect id or a name; a name is resolved to its id first.
type onePasswordConfig struct {
	// URL is the 1Password Connect server base URL, for example https://connect.example.com.
	URL string `json:"url"`
	// Token is the Connect API token presented as a bearer credential.
	Token string `json:"token"`
	// Vault is the vault id or name holding the item.
	Vault string `json:"vault"`
	// Item is the item id or title to read.
	Item string `json:"item"`
	// Field selects which field to return by its label or id. Empty returns the item's password field.
	Field string `json:"field,omitempty"`
}

// opRef is the id-and-name shape a Connect list returns for a vault or item, enough to resolve a name
// to its id.
type opRef struct {
	// ID is the Connect identifier.
	ID string `json:"id"`
	// Name is a vault's name.
	Name string `json:"name"`
	// Title is an item's title.
	Title string `json:"title"`
}

// opItem is the subset of a Connect item that carries its fields.
type opItem struct {
	// Fields are the item's fields, one of which holds the wanted value.
	Fields []opField `json:"fields"`
}

// opField is one field of a Connect item.
type opField struct {
	// ID is the field id.
	ID string `json:"id"`
	// Label is the field's human label, such as password or credential.
	Label string `json:"label"`
	// Value is the field's value.
	Value string `json:"value"`
	// Purpose marks a well-known field, PASSWORD for the item's password.
	Purpose string `json:"purpose"`
}

// resolveOnePassword reads a field of a 1Password item through a Connect server and returns its value,
// so a source resolves from 1Password at run time with no op CLI on the runner. It resolves the vault
// and item names to ids, fetches the item, and returns the requested field, defaulting to the password.
func resolveOnePassword(ctx context.Context, config string) (string, error) {
	var cfg onePasswordConfig
	if err := json.Unmarshal([]byte(config), &cfg); err != nil {
		return "", fmt.Errorf("%w: onepassword config is not valid JSON", ErrResolve)
	}
	if cfg.URL == "" || cfg.Token == "" || cfg.Vault == "" || cfg.Item == "" {
		return "", fmt.Errorf("%w: onepassword config needs url, token, vault, and item", ErrResolve)
	}
	base := strings.TrimRight(cfg.URL, "/")

	vaultID, err := opResolveID(ctx, base, cfg.Token, base+"/v1/vaults", "name", cfg.Vault)
	if err != nil {
		return "", err
	}
	itemsURL := base + "/v1/vaults/" + url.PathEscape(vaultID) + "/items"
	itemID, err := opResolveID(ctx, base, cfg.Token, itemsURL, "title", cfg.Item)
	if err != nil {
		return "", err
	}

	body, err := opGet(ctx, itemsURL+"/"+url.PathEscape(itemID), cfg.Token)
	if err != nil {
		return "", err
	}
	var item opItem
	if err := json.Unmarshal(body, &item); err != nil {
		return "", fmt.Errorf("%w: onepassword item is not valid JSON", ErrResolve)
	}
	return opFieldValue(item, cfg.Field)
}

// opResolveID returns value unchanged when it is already a Connect id, otherwise it looks up the id by
// filtering the list endpoint on field. An ambiguous name is an error so a duplicate title never
// resolves to the wrong secret.
func opResolveID(ctx context.Context, base, token, listURL, field, value string) (string, error) {
	if looksLikeOPID(value) {
		return value, nil
	}
	filter := field + ` eq "` + value + `"`
	body, err := opGet(ctx, listURL+"?filter="+url.QueryEscape(filter), token)
	if err != nil {
		return "", err
	}
	var refs []opRef
	if err := json.Unmarshal(body, &refs); err != nil {
		return "", fmt.Errorf("%w: onepassword list is not valid JSON", ErrResolve)
	}
	switch len(refs) {
	case 0:
		return "", fmt.Errorf("%w: onepassword %s %q not found", ErrResolve, field, value)
	case 1:
		return refs[0].ID, nil
	default:
		return "", fmt.Errorf("%w: onepassword %s %q is ambiguous, use its id", ErrResolve, field, value)
	}
}

// opGet performs a guarded bearer-authenticated GET against a Connect URL and returns the response
// body, mapping a non-200 to a resolve error.
func opGet(ctx context.Context, rawURL, token string) ([]byte, error) {
	if err := checkResolveURL(rawURL); err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", ErrResolve, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	resp, err := safeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("%w: onepassword request failed: %s", ErrResolve, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, httpMaxBody))
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%w: onepassword returned %s", ErrResolve, resp.Status)
	}
	return body, nil
}

// opFieldValue returns the wanted field's value from an item. An empty want returns the password
// field; otherwise it matches a field by label, case-insensitively, or by id.
func opFieldValue(item opItem, want string) (string, error) {
	for _, f := range item.Fields {
		if want == "" {
			if f.Purpose == "PASSWORD" && f.Value != "" {
				return f.Value, nil
			}
			continue
		}
		if strings.EqualFold(f.Label, want) || f.ID == want {
			if f.Value == "" {
				return "", fmt.Errorf("%w: onepassword field %q has no value", ErrResolve, want)
			}
			return f.Value, nil
		}
	}
	if want == "" {
		return "", fmt.Errorf("%w: onepassword item has no password field", ErrResolve)
	}
	return "", fmt.Errorf("%w: onepassword item has no field %q", ErrResolve, want)
}

// looksLikeOPID reports whether s has the shape of a Connect id, 26 lowercase base32 characters, so a
// value that is already an id skips the name lookup.
func looksLikeOPID(s string) bool {
	if len(s) != 26 {
		return false
	}
	for _, c := range s {
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') {
			return false
		}
	}
	return true
}
