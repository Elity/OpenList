package strm_sync

import (
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"
)

// The web UI does not render the `help:` text the backend sends. It uses it as
// a flag -- the tips row is `<Show when={field.help}>` -- and then looks the
// text up in a dictionary compiled into the frontend bundle, keyed by driver
// name and *JSON tag*:
//
//	label   drivers.StrmSync.<tag>
//	tips    drivers.StrmSync.<tag>-tips
//	option  drivers.StrmSync.<tag>s.<option>
//
// A key that misses is not reported anywhere. The translator falls back to
// capitalising the last path segment, so `drivers.StrmSync.localMode` renders
// as "LocalMode" -- which looks enough like a real label that i18n.json shipped
// keyed by Go field name and nobody noticed until the form was on screen.
//
// This test is the thing that would have caught it: the dictionary and the
// struct are two halves of one contract, and nothing else checks they agree.

type i18nField struct {
	tag     string
	hasHelp bool
	options []string // nil unless the field is a select
}

func additionFields(t *testing.T) []i18nField {
	t.Helper()
	rt := reflect.TypeOf(Addition{})
	fields := make([]i18nField, 0, rt.NumField())
	for i := 0; i < rt.NumField(); i++ {
		f := rt.Field(i)
		tag, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		if tag == "" || tag == "-" {
			t.Fatalf("%s has no json tag, so the form has no key to look it up by", f.Name)
		}
		field := i18nField{tag: tag, hasHelp: f.Tag.Get("help") != ""}
		if f.Tag.Get("type") == "select" {
			field.options = strings.Split(f.Tag.Get("options"), ",")
		}
		fields = append(fields, field)
	}
	return fields
}

// wantedKeys is every key the form will ask the dictionary for, mapped to
// whether the value must be an object (a select's options) rather than a
// string. Anything outside this set is a key nothing ever reads.
func wantedKeys(fields []i18nField) map[string]bool {
	want := make(map[string]bool, len(fields)*2)
	for _, f := range fields {
		want[f.tag] = false
		if f.hasHelp {
			want[f.tag+"-tips"] = false
		}
		if f.options != nil {
			want[f.tag+"s"] = true
		}
	}
	return want
}

func loadI18n(t *testing.T) map[string]map[string]json.RawMessage {
	t.Helper()
	raw, err := os.ReadFile("i18n.json")
	if err != nil {
		t.Fatalf("read i18n.json: %v", err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("parse i18n.json: %v", err)
	}
	delete(doc, "_comment")
	if len(doc) == 0 {
		t.Fatal("i18n.json has no languages")
	}
	out := make(map[string]map[string]json.RawMessage, len(doc))
	for lang, body := range doc {
		var dict map[string]json.RawMessage
		if err := json.Unmarshal(body, &dict); err != nil {
			t.Fatalf("parse %s: %v", lang, err)
		}
		out[lang] = dict
	}
	return out
}

func TestEveryFieldTheFormRendersHasATranslation(t *testing.T) {
	fields := additionFields(t)
	want := wantedKeys(fields)

	for lang, dict := range loadI18n(t) {
		for key, wantObject := range want {
			value, ok := dict[key]
			if !ok {
				t.Errorf("%s: missing %q -- the form would render the capitalised key instead", lang, key)
				continue
			}
			isObject := len(value) > 0 && value[0] == '{'
			if isObject != wantObject {
				t.Errorf("%s: %q is an object=%v, want object=%v", lang, key, isObject, wantObject)
			}
		}
	}
}

func TestTheDictionaryHasNoKeyTheFormNeverAsksFor(t *testing.T) {
	fields := additionFields(t)
	want := wantedKeys(fields)

	for lang, dict := range loadI18n(t) {
		for key := range dict {
			if _, ok := want[key]; !ok {
				// Either the field was renamed and this is stale, or it is a
				// `-tips` for a field carrying no `help:` tag, whose row the
				// form never renders. Both are translations nobody will read.
				t.Errorf("%s: %q is not a key the form looks up", lang, key)
			}
		}
	}
}

func TestEverySelectOptionIsTranslated(t *testing.T) {
	fields := additionFields(t)

	for lang, dict := range loadI18n(t) {
		for _, f := range fields {
			if f.options == nil {
				continue
			}
			var options map[string]string
			if err := json.Unmarshal(dict[f.tag+"s"], &options); err != nil {
				t.Errorf("%s: %ss is not an object of strings: %v", lang, f.tag, err)
				continue
			}
			declared := make(map[string]bool, len(f.options))
			for _, option := range f.options {
				declared[option] = true
				if _, ok := options[option]; !ok {
					t.Errorf("%s: %ss is missing %q", lang, f.tag, option)
				}
			}
			for option := range options {
				if !declared[option] {
					t.Errorf("%s: %ss translates %q, which is not one of the field's options", lang, f.tag, option)
				}
			}
		}
	}
}

func TestTheLanguagesAgreeOnTheirKeys(t *testing.T) {
	dicts := loadI18n(t)
	for _, required := range []string{"en", "zh-CN", "zh-TW"} {
		if _, ok := dicts[required]; !ok {
			t.Errorf("no %s translations; the injector skips any chunk it has no language for", required)
		}
	}

	var reference string
	for lang := range dicts {
		if reference == "" || lang < reference {
			reference = lang
		}
	}
	for lang, dict := range dicts {
		if lang == reference {
			continue
		}
		for key := range dicts[reference] {
			if _, ok := dict[key]; !ok {
				t.Errorf("%s is missing %q, which %s has", lang, key, reference)
			}
		}
		for key := range dict {
			if _, ok := dicts[reference][key]; !ok {
				t.Errorf("%s has %q, which %s does not", lang, key, reference)
			}
		}
	}
}
