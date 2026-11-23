package collect

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/pgrundev/pgbot/internal/conn"
	"github.com/pgrundev/pgbot/internal/findings"
	"github.com/pgrundev/pgbot/internal/model"
)

func TestIntegration_sequencesCollectorDirectionCycleAndBoundedRiskOrder(t *testing.T) {
	superDSN := os.Getenv("PGBOT_TEST_SUPERUSER_DSN")
	readDSN := os.Getenv("PGBOT_TEST_DSN")
	if superDSN == "" || readDSN == "" {
		t.Skip("set PGBOT_TEST_SUPERUSER_DSN and PGBOT_TEST_DSN to run the disposable sequence fixture")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, superDSN)
	if err != nil {
		t.Fatalf("admin connect: %v", err)
	}
	database := fmt.Sprintf("pgbot_seq_%d_%d", os.Getpid(), time.Now().UnixNano())
	quotedDatabase := pgx.Identifier{database}.Sanitize()
	var fixture *pgx.Conn
	var target *conn.Target
	databaseCreated := false
	t.Cleanup(func() {
		cleanupCtx, cleanupCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cleanupCancel()
		if target != nil {
			target.Close()
		}
		if fixture != nil {
			if err := fixture.Close(cleanupCtx); err != nil {
				t.Errorf("close fixture connection: %v", err)
			}
		}
		if databaseCreated {
			if _, err := admin.Exec(cleanupCtx, "DROP DATABASE "+quotedDatabase+" WITH (FORCE)"); err != nil {
				t.Errorf("drop disposable database %s: %v", database, err)
			}
		}
		if err := admin.Close(cleanupCtx); err != nil {
			t.Errorf("close admin connection: %v", err)
		}
	})

	if _, err := admin.Exec(ctx, "CREATE DATABASE "+quotedDatabase); err != nil {
		t.Fatalf("create disposable database: %v", err)
	}
	databaseCreated = true
	fixtureConfig, err := pgx.ParseConfig(superDSN)
	if err != nil {
		t.Fatalf("parse superuser DSN: %v", err)
	}
	fixtureConfig.Database = database
	fixture, err = pgx.ConnectConfig(ctx, fixtureConfig)
	if err != nil {
		t.Fatalf("connect disposable database: %v", err)
	}

	for _, stmt := range []string{
		`CREATE SCHEMA seq_fixture`,
		`CREATE SEQUENCE seq_fixture.opposite_wrap_cycle AS bigint INCREMENT BY -1 MINVALUE -2147483648 MAXVALUE 9223372036854775807 START WITH -1 CYCLE`,
		`CREATE TABLE seq_fixture.opposite_wrap_owner (id int4)`,
		`ALTER SEQUENCE seq_fixture.opposite_wrap_cycle OWNED BY seq_fixture.opposite_wrap_owner.id`,
		`CREATE SEQUENCE seq_fixture.descending AS bigint INCREMENT BY -1 MINVALUE -100 MAXVALUE -1 START WITH -1 NO CYCLE`,
		`CREATE SEQUENCE seq_fixture.direct_narrow_cycle AS bigint INCREMENT BY 1 MINVALUE 1 MAXVALUE 9223372036854775807 START WITH 1 CYCLE`,
		`CREATE TABLE seq_fixture.direct_narrow_owner (id int4)`,
		`ALTER SEQUENCE seq_fixture.direct_narrow_cycle OWNED BY seq_fixture.direct_narrow_owner.id`,
		`CREATE SEQUENCE seq_fixture.unsafe_a AS bigint INCREMENT BY 1 MINVALUE 1 MAXVALUE 101 START WITH 1 NO CYCLE`,
		`CREATE SEQUENCE seq_fixture.unsafe_b AS bigint INCREMENT BY 1 MINVALUE 1 MAXVALUE 101 START WITH 1 NO CYCLE`,
	} {
		if _, err := fixture.Exec(ctx, stmt); err != nil {
			t.Fatalf("fixture statement %q: %v", stmt, err)
		}
	}
	for name, value := range map[string]int64{
		"opposite_wrap_cycle": -2_000_000_000,
		"descending":          -95,
		"direct_narrow_cycle": 2_000_000_000,
		"unsafe_a":            91,
		"unsafe_b":            91,
	} {
		if _, err := fixture.Exec(ctx, `SELECT setval($1::regclass, $2, true)`, "seq_fixture."+name, value); err != nil {
			t.Fatalf("position %s: %v", name, err)
		}
	}
	for i := 0; i < 55; i++ {
		name := fmt.Sprintf("safe_cycle_%02d", i)
		qualified := pgx.Identifier{"seq_fixture", name}.Sanitize()
		if _, err := fixture.Exec(ctx, "CREATE SEQUENCE "+qualified+" AS bigint MINVALUE 1 MAXVALUE 100 START WITH 1 CYCLE"); err != nil {
			t.Fatalf("create %s: %v", name, err)
		}
		if _, err := fixture.Exec(ctx, `SELECT setval($1::regclass, 99, true)`, "seq_fixture."+name); err != nil {
			t.Fatalf("position %s: %v", name, err)
		}
	}
	if _, err := fixture.Exec(ctx, `GRANT USAGE ON SCHEMA seq_fixture TO PUBLIC; GRANT SELECT ON ALL SEQUENCES IN SCHEMA seq_fixture TO PUBLIC`); err != nil {
		t.Fatalf("grant fixture visibility: %v", err)
	}

	target, err = conn.ConnectDB(ctx, readDSN, database)
	if err != nil {
		t.Fatalf("collector connect: %v", err)
	}
	sample, err := (sequencesCollector{}).Sample(ctx, target, target.Caps)
	if err != nil {
		t.Fatalf("read-only sequence sample: %v", err)
	}
	c := &model.Context{}
	(sequencesCollector{}).Assemble(c, target.Caps, sampled{A: sample}, 0, Options{})
	if c.Sequences == nil || len(c.Sequences.Items) != 50 {
		t.Fatalf("SQL must return a bounded top 50, got %+v", c.Sequences)
	}
	wantPrefix := []string{"opposite_wrap_cycle", "descending", "direct_narrow_cycle", "unsafe_a", "unsafe_b"}
	gotPrefix := make([]string, len(wantPrefix))
	for i := range wantPrefix {
		gotPrefix[i] = c.Sequences.Items[i].Name
	}
	if !reflect.DeepEqual(gotPrefix, wantPrefix) {
		t.Fatalf("model item order must match bounded SQL risk selection: got %v want %v", gotPrefix, wantPrefix)
	}

	metadata := sequenceMetadata(t, c.Sequences.Items[0])
	if metadata["increment"] != float64(-1) || metadata["cycle"] != true || metadata["column_limited"] != true {
		t.Fatalf("opposite-end column limit metadata was not preserved: %v", metadata)
	}
	seen := map[string]bool{}
	for _, object := range hasSequenceFinding(t, findings.Compute(c)).Objects {
		seen[object] = true
	}
	for _, name := range wantPrefix {
		if !seen["seq_fixture."+name] {
			t.Errorf("missing at-risk sequence %s from finding: %v", name, seen)
		}
	}
	for object := range seen {
		if len(object) >= len("seq_fixture.safe_cycle_") && object[:len("seq_fixture.safe_cycle_")] == "seq_fixture.safe_cycle_" {
			t.Errorf("safe in-range cycle reported as numeric exhaustion: %s", object)
		}
	}
}

func sequenceMetadata(t *testing.T, item model.SequenceUsage) map[string]any {
	t.Helper()
	blob, err := json.Marshal(item)
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(blob, &raw); err != nil {
		t.Fatal(err)
	}
	return raw
}

func hasSequenceFinding(t *testing.T, fs []model.Finding) *model.Finding {
	t.Helper()
	for i := range fs {
		if fs[i].ID == "sequence_exhaustion" {
			return &fs[i]
		}
	}
	t.Fatal("expected sequence_exhaustion")
	return nil
}
