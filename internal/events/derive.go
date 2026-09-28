// Package events derives what CHANGED in a database's schema, configuration and
// lifecycle between two runs — the timeline a future correlation engine needs.
// Deltas say a number moved; events say something happened.
package events

import (
	"regexp"
	"strings"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

// inferredConfidence is used for schema changes we only know happened somewhere
// in the (prevAt, curAt) window. Real-timestamp events (reset/restart) get 1.0.
const inferredConfidence = 0.5

// redactSetting matches setting names whose VALUES must never be stored.
var redactSetting = regexp.MustCompile(`(?i)(password|_key$|^ssl_.*_file$)`)

// Derive compares the current run against the previous one and returns the
// events between them. prevSchema is the previous schema fingerprint (loaded
// from the store; the Context's Schema is not serialized). prevAt is when the
// previous run was taken; cur.CollectedAt is now.
func Derive(cur *model.Context, prevSchema []model.SchemaObject, prevSettings map[string]string, prevAt time.Time) []model.Event {
	var out []model.Event
	window := func(e model.Event) model.Event {
		a, b := prevAt, cur.CollectedAt
		e.OccurredAfter, e.OccurredBefore = &a, &b
		e.Confidence = inferredConfidence
		return e
	}

	// Lifecycle events carry REAL timestamps (confidence 1.0).
	if cur.Window.StatsResetAt != nil && after(cur.Window.StatsResetAt, prevAt) {
		out = append(out, model.Event{Kind: "stats.reset", Object: cur.Server.Database,
			OccurredAfter: cur.Window.StatsResetAt, OccurredBefore: cur.Window.StatsResetAt, Confidence: 1.0})
	}
	if cur.Window.PostmasterStartAt != nil && after(cur.Window.PostmasterStartAt, prevAt) {
		out = append(out, model.Event{Kind: "server.restarted", Object: cur.Server.Database,
			OccurredAfter: cur.Window.PostmasterStartAt, OccurredBefore: cur.Window.PostmasterStartAt, Confidence: 1.0})
	}

	// Schema diff (inferred window).
	if cur.Schema != nil {
		out = append(out, schemaEvents(prevSchema, cur.Schema.Objects, window)...)
	}

	// Config diff (inferred window).
	out = append(out, configEvents(prevSettings, currentSettings(cur), window)...)

	return out
}

func schemaEvents(prev, cur []model.SchemaObject, window func(model.Event) model.Event) []model.Event {
	if len(prev) == 0 {
		return nil // no prior fingerprint — nothing to compare, first observation
	}
	prevByID := index(prev)
	curByID := index(cur)
	var out []model.Event

	for id, o := range curByID {
		p, existed := prevByID[id]
		if !existed {
			out = append(out, window(model.Event{Kind: created(o.Kind), Object: o.Identity, After: o.Definition}))
			continue
		}
		if p.DefinitionHash != o.DefinitionHash {
			out = append(out, window(model.Event{Kind: changed(o.Kind), Object: o.Identity, Before: p.Definition, After: o.Definition}))
		}
	}
	for id, p := range prevByID {
		if _, ok := curByID[id]; !ok {
			out = append(out, window(model.Event{Kind: dropped(p.Kind), Object: p.Identity, Before: p.Definition}))
		}
	}
	return out
}

func configEvents(prev, cur map[string]string, window func(model.Event) model.Event) []model.Event {
	if prev == nil || cur == nil {
		return nil
	}
	var out []model.Event
	seen := map[string]bool{}
	for name, v := range cur {
		seen[name] = true
		if pv, ok := prev[name]; ok && pv != v {
			out = append(out, window(model.Event{Kind: "config.changed", Object: name,
				Before: redact(name, pv), After: redact(name, v)}))
		} else if !ok {
			out = append(out, window(model.Event{Kind: "config.changed", Object: name, After: redact(name, v)}))
		}
	}
	for name, pv := range prev {
		if !seen[name] {
			out = append(out, window(model.Event{Kind: "config.changed", Object: name, Before: redact(name, pv)}))
		}
	}
	return out
}

func currentSettings(c *model.Context) map[string]string {
	if c.Settings == nil {
		return nil
	}
	return c.Settings.Overrides
}

func redact(name, val string) string {
	if redactSetting.MatchString(name) {
		return "«redacted»"
	}
	return val
}

func index(objs []model.SchemaObject) map[string]model.SchemaObject {
	m := make(map[string]model.SchemaObject, len(objs))
	for _, o := range objs {
		m[o.Kind+":"+o.Identity] = o
	}
	return m
}

func after(t *time.Time, ref time.Time) bool { return t != nil && t.After(ref) }

// created/dropped/changed map an object kind to its event kind.
func created(kind string) string { return "schema." + objectNoun(kind) + "_created" }
func dropped(kind string) string { return "schema." + objectNoun(kind) + "_dropped" }
func changed(kind string) string {
	switch kind {
	case "column":
		return "schema.column_type_changed"
	case "extension":
		return "schema.extension_changed"
	case "constraint":
		return "schema.constraint_changed"
	default:
		return "schema." + objectNoun(kind) + "_changed"
	}
}

func objectNoun(kind string) string {
	// created/dropped nouns: index/table/column/constraint/extension/sequence
	if kind == "" {
		return "object"
	}
	return strings.ToLower(kind)
}
