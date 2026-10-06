package forgelink

import (
	"context"
	"encoding/json"
	"fmt"
)

// Actions a link's chain entry records.
const (
	// ActionLinked records a link being made.
	ActionLinked = "linked"
	// ActionUnlinked records a link ending, by its person or with its account.
	ActionUnlinked = "unlinked"
)

// ChangeRecord is the body a link's or an unlink's chain entry commits to. It names the forge
// account by its numeric id and never by its login, and it carries no token.
type ChangeRecord struct {
	// Action is linked or unlinked.
	Action string `json:"action"`
	// LinkID is the link's id.
	LinkID string `json:"link_id"`
	// UserID is the SwitchTender account.
	UserID string `json:"user_id"`
	// Provider is github or gitlab.
	Provider string `json:"provider"`
	// APIURL is the forge's REST API base.
	APIURL string `json:"api_url"`
	// ForgeUserID is the forge's numeric id of the account.
	ForgeUserID int64 `json:"forge_user_id"`
}

// ChangeBody returns the JSON body the chain entry for action on l commits to.
func ChangeBody(action string, l *Link) ([]byte, error) {
	body, err := json.Marshal(ChangeRecord{
		Action: action, LinkID: l.ID, UserID: l.UserID, Provider: l.Provider, APIURL: l.APIURL,
		ForgeUserID: l.ForgeUserID,
	})
	if err != nil {
		return nil, fmt.Errorf("encode forge link record: %w", err)
	}
	return body, nil
}

// ChangePath returns the chain path of action on the link with id.
func ChangePath(id, action string) string {
	return "/me/forge-links/" + id + "/" + action
}

// UnlinkUser ends every link of the SwitchTender account userID, recording each with record before
// removing it, and returns how many it removed. It fails closed one link at a time: a link whose
// unlink record cannot be written is kept, with every link after it, and the error is returned, so
// the chain never shows a link ending that did not and never misses one that did.
func UnlinkUser(ctx context.Context, store Store, userID string, record func(*Link) error) (int, error) {
	if store == nil {
		return 0, nil
	}
	links, err := store.ForUser(ctx, userID)
	if err != nil {
		return 0, fmt.Errorf("list forge links: %w", err)
	}
	removed := 0
	for _, l := range links {
		if err := record(l); err != nil {
			return removed, fmt.Errorf("record the forge link's end: %w", err)
		}
		if _, err := store.Delete(ctx, userID, l.ID); err != nil {
			return removed, fmt.Errorf("remove forge link: %w", err)
		}
		removed++
	}
	return removed, nil
}
