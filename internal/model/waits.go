package model

// WaitsSchemaVersion versions the `pgbot waits --json` document, independently
// of the Context schema — same policy as advise and why.
const WaitsSchemaVersion = "1.0.0"

// WaitStudy is the result of one bounded wait-sampling window: where database
// time went, who was blocking whom, and exactly how much evidence backs each
// claim. Everything here is derived from SAMPLES — shares of an observed
// window, never measured durations. The only exact numbers are ages read
// directly from the server (xact_start, query_start).
type WaitStudy struct {
	SchemaVersion string  `json:"waits_schema_version"`
	Exactness     string  `json:"exactness"` // always "sampled" in native mode
	WindowSeconds float64 `json:"window_seconds"`
	Hz            int     `json:"hz"`

	// Coverage: how much of the intended sampling actually happened.
	Polls             int `json:"polls"`
	PollFailures      int `json:"poll_failures"`
	LockSnapshots     int `json:"lock_snapshots"`
	LockSnapshotFails int `json:"lock_snapshot_failures"`

	AAS     float64 `json:"avg_active_sessions"` // samples / successful polls
	Thin    bool    `json:"thin"`                // under WaitMinSamples — every conclusion demoted
	Partial string  `json:"partial,omitempty"`   // e.g. role lacks pg_monitor

	Profile   *WaitProfile   `json:"profile"`                        // reuses the inspect wait-profile shape
	Sessions  []SessionWaits `json:"sessions,omitempty"`             // per-PID rollup
	Blockers  []Blocker      `json:"blockers,omitempty"`             // sustained evidence only
	Transient []Blocker      `json:"transient_lock_waits,omitempty"` // seen, but not evidence of a root cause
}

// SessionWaits is one backend's share of the sampled window.
type SessionWaits struct {
	PID        int     `json:"pid"`
	User       string  `json:"user,omitempty"`
	DB         string  `json:"db,omitempty"`
	App        string  `json:"app,omitempty"`
	Count      int     `json:"count"`
	Share      float64 `json:"share"` // of all samples in the window
	TopType    string  `json:"top_type,omitempty"`
	TopEvent   string  `json:"top_event,omitempty"`
	SampleText string  `json:"sample_text,omitempty"` // scrubbed unless the operator opted out
}

// Blocker is a lock holder observed across slow-plane snapshots. Sustained is
// the evidence gate: ≥3 snapshots, or ≥2 with the victim's own sampled time
// majority-Lock. A non-sustained holder is reported transient, never named as
// a root cause.
type Blocker struct {
	HolderPID      int             `json:"holder_pid"`
	HolderState    string          `json:"holder_state,omitempty"`
	HolderXactAgeS float64         `json:"holder_xact_age_s"` // max observed; exact, read from the server
	HolderQuery    string          `json:"holder_query,omitempty"`
	HolderUser     string          `json:"holder_user,omitempty"`
	HolderApp      string          `json:"holder_app,omitempty"`
	Observations   int             `json:"observations"` // slow-plane snapshots this holder appeared in
	Sustained      bool            `json:"sustained"`
	Victims        []BlockedVictim `json:"victims,omitempty"`
}

// BlockedVictim is one backend a blocker held up.
type BlockedVictim struct {
	PID       int     `json:"pid"`
	Query     string  `json:"query,omitempty"`
	WaitEvent string  `json:"wait_event,omitempty"`
	MaxWaitS  float64 `json:"max_wait_s"` // exact, from query_start
}
