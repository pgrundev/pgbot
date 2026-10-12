package render

import (
	"bytes"
	"errors"
	"io"
	"reflect"
	"strings"
	"testing"

	"github.com/pgrundev/pgbot/internal/advisor"
	"github.com/pgrundev/pgbot/internal/model"
)

var errReportOutput = errors.New("report output rejected")

type boundedReportWriter struct {
	limit int
	err   error
}

func (w boundedReportWriter) Write(p []byte) (int, error) {
	n := w.limit
	if n > len(p) {
		n = len(p)
	}
	return n, w.err
}

func callReport(t *testing.T, report any, w io.Writer, input any) error {
	t.Helper()
	out := reflect.ValueOf(report).Call([]reflect.Value{reflect.ValueOf(w), reflect.ValueOf(input)})
	if len(out) != 1 {
		t.Fatalf("renderer returned %d values, want one error", len(out))
	}
	if out[0].IsNil() {
		return nil
	}
	err, ok := out[0].Interface().(error)
	if !ok {
		t.Fatalf("renderer returned %T, want error", out[0].Interface())
	}
	return err
}

func largeDiffInput() DiffInput {
	changes := make([]model.Delta, 600)
	for i := range changes {
		changes[i] = model.Delta{
			Subject:  strings.Repeat("query-", 24),
			Severity: model.SeverityWarn,
			Before:   1,
			After:    2,
			Note:     strings.Repeat("changed ", 32),
		}
	}
	return DiffInput{Database: "app", Fingerprint: "abcdef", Deltas: &model.Deltas{Changes: changes}}
}

func largeAdvisorInput() AdvisorInput {
	recs := make([]advisor.Recommendation, 300)
	for i := range recs {
		recs[i] = advisor.Recommendation{
			Schema: "public", Table: "orders", IndexDDL: "CREATE INDEX ON public.orders (customer_id)",
			QueryText: strings.Repeat("SELECT * FROM orders WHERE customer_id = $1 ", 8),
			Calls:     1000, SharePct: 25, CostBefore: 1000, CostAfter: 100,
			Caveats: []string{strings.Repeat("writes also maintain this index ", 8)},
		}
	}
	return AdvisorInput{Database: "app", VersionNum: 160000, Recommendations: recs, Planned: 300, Candidates: 300}
}

func TestTextReports_PropagateWriterErrors(t *testing.T) {
	tests := []struct {
		name   string
		report any
		input  any
	}{
		{name: "diff empty", report: DiffReport, input: DiffInput{Database: "app"}},
		{name: "diff reset only", report: DiffReport, input: DiffInput{Database: "app", ResetReason: "server restarted"}},
		{name: "advisor empty", report: AdvisorReport, input: AdvisorInput{Database: "app", VersionNum: 160000}},
		{name: "diff large", report: DiffReport, input: largeDiffInput()},
		{name: "advisor large", report: AdvisorReport, input: largeAdvisorInput()},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := callReport(t, tc.report, boundedReportWriter{limit: 17, err: errReportOutput}, tc.input)
			if !errors.Is(err, errReportOutput) {
				t.Fatalf("error = %v, want errors.Is(..., %v)", err, errReportOutput)
			}
		})
	}
}

func TestTextReports_RejectSilentShortWrites(t *testing.T) {
	for _, tc := range []struct {
		name   string
		report any
		input  any
	}{
		{name: "diff", report: DiffReport, input: largeDiffInput()},
		{name: "advisor", report: AdvisorReport, input: largeAdvisorInput()},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := callReport(t, tc.report, boundedReportWriter{limit: 17}, tc.input)
			if !errors.Is(err, io.ErrShortWrite) {
				t.Fatalf("error = %v, want io.ErrShortWrite", err)
			}
		})
	}
}

func TestTextReports_PreserveNormalOutput(t *testing.T) {
	var diffOut bytes.Buffer
	if err := callReport(t, DiffReport, &diffOut, DiffInput{Database: "app", Deltas: &model.Deltas{}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(diffOut.String(), "nothing material changed") {
		t.Fatalf("unexpected diff output:\n%s", diffOut.String())
	}

	var advisorOut bytes.Buffer
	if err := callReport(t, AdvisorReport, &advisorOut, AdvisorInput{Database: "app", VersionNum: 160000}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(advisorOut.String(), "no validated index recommendations") {
		t.Fatalf("unexpected advisor output:\n%s", advisorOut.String())
	}

	var largeDiff, largeAdvisor bytes.Buffer
	if err := callReport(t, DiffReport, &largeDiff, largeDiffInput()); err != nil {
		t.Fatal(err)
	}
	if err := callReport(t, AdvisorReport, &largeAdvisor, largeAdvisorInput()); err != nil {
		t.Fatal(err)
	}
	if largeDiff.Len() < 64<<10 || largeAdvisor.Len() < 64<<10 {
		t.Fatalf("large reports were unexpectedly small: diff=%d advisor=%d", largeDiff.Len(), largeAdvisor.Len())
	}
}
