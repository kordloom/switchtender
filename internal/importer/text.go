package importer

import (
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"strings"

	"github.com/kordloom/switchtender/internal/util"
)

// ErrUnstorableText is returned by Apply for a plan holding a NUL byte or text that is not valid
// UTF-8 in any object it would write.
//
// An export is somebody else's file, and JSON and YAML both spell a NUL as an escape. SQLite keeps
// the byte and PostgreSQL refuses it, so on PostgreSQL the import failed partway with a 500, after
// the objects ahead of the bad one were already written. The plan is checked before anything is.
var ErrUnstorableText = errors.New("the export holds text no store can keep")

// textNameCap bounds how much of an object's name a refusal quotes.
const textNameCap = 80

// checkText returns ErrUnstorableText naming the first planned object, and its field, that holds a
// NUL byte or text that is not valid UTF-8, or nil when every field of every object is storable.
func (p *Plan) checkText() error {
	groups := []struct {
		kind    string
		objects any
	}{{"project", p.Projects}, {"inventory", p.Inventories}, {"inventory source", p.Sources},
		{"template", p.Templates}, {"schedule", p.Schedules}, {"credential", p.Credentials},
		{"credential type", p.CredentialTypes}, {"notification target", p.Notifications},
		{"notification attachment", p.Attachments}, {"organization", p.Orgs}}
	for _, g := range groups {
		objects := reflect.ValueOf(g.objects)
		for i := 0; i < objects.Len(); i++ {
			field, found := unstorableField(objects.Index(i), "")
			if !found {
				continue
			}
			name := ""
			if obj := reflect.Indirect(objects.Index(i)); obj.Kind() == reflect.Struct {
				if n := obj.FieldByName("Name"); n.IsValid() && n.Kind() == reflect.String {
					name = " " + strconv.Quote(util.Clip(util.SafeText(n.String()), textNameCap))
				}
			}
			return fmt.Errorf("%w: the %s%s holds a NUL byte or text that is not valid UTF-8 in %s",
				ErrUnstorableText, g.kind, name, field)
		}
	}
	return nil
}

// unstorableField walks v and returns the path of the first string in it holding text no store can
// keep, such as Hosts[2].Name, and whether one does. Only exported fields are walked, which is
// every field a store writes.
func unstorableField(v reflect.Value, path string) (string, bool) {
	switch v.Kind() {
	case reflect.Pointer, reflect.Interface:
		if v.IsNil() {
			return "", false
		}
		return unstorableField(v.Elem(), path)
	case reflect.String:
		return path, !util.IsSafeText(v.String())
	case reflect.Struct:
		t := v.Type()
		for i := 0; i < v.NumField(); i++ {
			if !t.Field(i).IsExported() {
				continue
			}
			name := t.Field(i).Name
			if path != "" {
				name = path + "." + name
			}
			if found, ok := unstorableField(v.Field(i), name); ok {
				return found, true
			}
		}
	case reflect.Slice, reflect.Array:
		if v.Type().Elem().Kind() == reflect.Uint8 {
			return "", false
		}
		for i := 0; i < v.Len(); i++ {
			if found, ok := unstorableField(v.Index(i), path+"["+strconv.Itoa(i)+"]"); ok {
				return found, true
			}
		}
	case reflect.Map:
		iter := v.MapRange()
		for iter.Next() {
			key := iter.Key()
			if key.Kind() == reflect.String && !util.IsSafeText(key.String()) {
				return strings.TrimPrefix(path+" key", " "), true
			}
			if found, ok := unstorableField(iter.Value(), path); ok {
				return found, true
			}
		}
	}
	return "", false
}
