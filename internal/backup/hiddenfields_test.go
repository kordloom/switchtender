package backup

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/kordloom/switchtender/internal/auth"
	"github.com/kordloom/switchtender/internal/credential"
	"github.com/kordloom/switchtender/internal/inventory"
	"github.com/kordloom/switchtender/internal/trigger"
	"github.com/kordloom/switchtender/internal/user"
)

// TestEveryHiddenFieldIsCarriedByItsDTO pins that a field an entity hides from JSON is carried
// explicitly in the backup, so a restore cannot silently drop it.
//
// The payload stores whole entities, so an ordinary field added later rides along with no work. A
// field tagged json:"-" does not: it is hidden precisely because it is secret, and the backup has to
// name it in a DTO or the value is simply absent from the file. Nothing goes wrong at backup time
// and nothing goes wrong at restore time either. The loss shows up later, as accounts that cannot
// sign in, webhooks that no longer authenticate, and credentials that decrypt to nothing, on the day
// somebody is restoring because their install is already gone.
//
// The existing round trip checks the fields hidden today. This checks the rule, so the next hidden
// field added to any of these entities fails here rather than in somebody's recovery.
func TestEveryHiddenFieldIsCarriedByItsDTO(t *testing.T) {
	t.Parallel()
	tests := []struct {
		Name   string
		Entity any
		DTO    any
	}{ // Test 0: The sealed execution secret.
		{"credential", credential.Credential{}, credentialDTO{}},
		// Test 1: The sealed dynamic-source configuration.
		{"inventory", inventory.Inventory{}, inventoryDTO{}},
		// Test 2: The webhook token hash and the body-signing secret.
		{"trigger", trigger.Trigger{}, triggerDTO{}},
		// Test 3: The password hash, without which no account can sign in after a restore.
		{"user", user.User{}, userDTO{}},
		// Test 4: The API token hash, without which every token is dead after a restore.
		{"token", auth.Token{}, tokenDTO{}},
	}
	for testNum, test := range tests {
		t.Run(fmt.Sprintf("test %d %s", testNum, test.Name), func(t *testing.T) {
			t.Parallel()
			entity := reflect.TypeOf(test.Entity)
			dto := reflect.TypeOf(test.DTO)

			// Only fields declared on the DTO itself count. The DTO embeds the entity, so a
			// promoted field would make every hidden field look carried when none of them are.
			carried := map[string]bool{}
			for i := 0; i < dto.NumField(); i++ {
				if f := dto.Field(i); !f.Anonymous {
					carried[f.Name] = true
				}
			}

			var hidden []string
			for i := 0; i < entity.NumField(); i++ {
				f := entity.Field(i)
				if f.Tag.Get("json") != "-" {
					continue
				}
				hidden = append(hidden, f.Name)
				if !carried[f.Name] {
					t.Errorf("%s.%s is hidden from JSON and no field on %s carries it, so a "+
						"restore drops it silently. Add it to the DTO and to apply().",
						entity.Name(), f.Name, dto.Name())
				}
			}
			if len(hidden) == 0 {
				t.Errorf("%s has no hidden fields, so this pairing no longer guards anything and "+
					"should be removed rather than left passing for the wrong reason", entity.Name())
			}
		})
	}
}
