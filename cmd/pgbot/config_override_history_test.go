package main

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/pgrundev/pgbot/internal/model"
)

func TestWithStore_ConfigOverrideHistory(t *testing.T) {
	baseTime := time.Date(2026, 9, 22, 9, 0, 0, 0, time.UTC)
	contextAt := func(fp string, at time.Time, overrides map[string]string) *model.Context {
		var settings *model.Settings
		if overrides != nil {
			settings = &model.Settings{Overrides: overrides}
		}
		return &model.Context{
			SchemaVersion: model.SchemaVersion,
			Fingerprint:   fp,
			CollectedAt:   at,
			Settings:      settings,
		}
	}
	run := func(t *testing.T, previous, current *model.Context) []model.Event {
		t.Helper()
		path := filepath.Join(t.TempDir(), "baselines.db")
		if previous != nil {
			withStore(path, previous)
		}
		withStore(path, current)
		return current.Events
	}
	findConfig := func(events []model.Event, name string) *model.Event {
		t.Helper()
		for i := range events {
			if events[i].Kind == "config.changed" && events[i].Object == name {
				return &events[i]
			}
		}
		return nil
	}

	t.Run("first override after collected defaults", func(t *testing.T) {
		previous := contextAt("first-override", baseTime, map[string]string{})
		current := contextAt("first-override", baseTime.Add(time.Minute), map[string]string{"work_mem": "64MB"})

		event := findConfig(run(t, previous, current), "work_mem")
		if event == nil {
			t.Fatal("first override was not derived from the stored empty baseline")
		}
		if event.Before != "" || event.After != "64MB" {
			t.Fatalf("event = %+v, want defaults -> 64MB", event)
		}
	})

	t.Run("no history", func(t *testing.T) {
		current := contextAt("no-history", baseTime, map[string]string{"work_mem": "64MB"})
		if event := findConfig(run(t, nil, current), "work_mem"); event != nil {
			t.Fatalf("first observation must not invent a change event: %+v", event)
		}
	})

	t.Run("settings unavailable in history", func(t *testing.T) {
		previous := contextAt("nil-settings", baseTime, nil)
		current := contextAt("nil-settings", baseTime.Add(time.Minute), map[string]string{"work_mem": "64MB"})
		if event := findConfig(run(t, previous, current), "work_mem"); event != nil {
			t.Fatalf("unknown previous settings must not invent a change event: %+v", event)
		}
	})

	t.Run("current settings unavailable", func(t *testing.T) {
		previous := contextAt("missing-current", baseTime, map[string]string{"work_mem": "64MB"})
		current := contextAt("missing-current", baseTime.Add(time.Minute), nil)
		if event := findConfig(run(t, previous, current), "work_mem"); event != nil {
			t.Fatalf("unavailable current settings must not invent a return to defaults: %+v", event)
		}
	})

	t.Run("current settings collector failed", func(t *testing.T) {
		previous := contextAt("failed-current", baseTime, map[string]string{"work_mem": "64MB"})
		current := contextAt("failed-current", baseTime.Add(time.Minute), nil)
		current.Settings = &model.Settings{Section: model.Section{Exactness: model.ExactnessUnavailable}}
		if event := findConfig(run(t, previous, current), "work_mem"); event != nil {
			t.Fatalf("a failed collector must not invent a return to defaults: %+v", event)
		}
	})

	t.Run("defaults unchanged", func(t *testing.T) {
		previous := contextAt("unchanged-defaults", baseTime, map[string]string{})
		current := contextAt("unchanged-defaults", baseTime.Add(time.Minute), map[string]string{})
		if events := run(t, previous, current); len(events) != 0 {
			t.Fatalf("unchanged defaults produced events: %+v", events)
		}
	})

	t.Run("return to default", func(t *testing.T) {
		previous := contextAt("return-default", baseTime, map[string]string{"work_mem": "64MB"})
		current := contextAt("return-default", baseTime.Add(time.Minute), map[string]string{})

		event := findConfig(run(t, previous, current), "work_mem")
		if event == nil {
			t.Fatal("return to default was not derived from stored history")
		}
		if event.Before != "64MB" || event.After != "" {
			t.Fatalf("event = %+v, want 64MB -> defaults", event)
		}
	})
}
