package assess

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// MS-10b2 item-3/4 anchors: the N/A dimension calibration and the report
// caliber annotation. PG payloads with populated dimensions must stay
// byte-identical on both faces (API JSON and rendered reports).

// populatedPGScan builds a scan where every dimension has at least one
// object — the PG parity precondition.
func populatedPGScan() *ScanResult {
	return &ScanResult{
		Tables:     []TableInfo{{Schema: "public", Name: "t1"}},
		Columns:    []ColumnInfo{{TableSchema: "public", TableName: "t1", ColumnName: "id", DataType: "integer", IsPrimary: true}},
		Indexes:    []IndexInfo{{TableName: "t1", Name: "pk", IndexType: "btree", IsPrimary: true}},
		Views:      []ViewInfo{{Schema: "public", Name: "v1", Definition: "SELECT 1"}},
		Functions:  []FunctionInfo{{Schema: "public", Name: "f1", Language: "sql"}},
		Triggers:   []TriggerInfo{{TableName: "t1", Name: "trg1"}},
		Enums:      []EnumInfo{{Schema: "public", Name: "e1", Values: []string{"a"}}},
		Extensions: []ExtensionInfo{{Name: "plpgsql"}},
		Sequences:  []SequenceInfo{{Schema: "public", Name: "s1"}},
	}
}

// TestEmptyDimensionsNA pins the N/A contract: a MySQL-shaped scan (no
// enums/extensions/sequences) marks those dimensions not-applicable with
// score 0 and the explicit "applicable": false JSON field, while
// populated dimensions keep the legacy shape (field omitted).
func TestEmptyDimensionsNA(t *testing.T) {
	res := &ScanResult{
		Tables:  []TableInfo{{Schema: "db1", Name: "t1"}},
		Columns: []ColumnInfo{{TableSchema: "db1", TableName: "t1", ColumnName: "id", DataType: "int", IsPrimary: true}},
		Indexes: []IndexInfo{{TableName: "t1", Name: "PRIMARY", IndexType: "btree", IsPrimary: true}},
		Views:   []ViewInfo{{Schema: "db1", Name: "v1", Definition: "SELECT 1"}},
	}
	dims := NewAssessorFor("mysql").Assess(res)

	byDim := map[string]DimensionResult{}
	for _, d := range dims {
		byDim[d.Dimension] = d
	}
	for _, dim := range []string{DimFunction, DimTrigger, DimCustomType, DimExtension, DimSequence} {
		if byDim[dim].IsApplicable() {
			t.Errorf("%s must be N/A on an empty object set", dim)
		}
		if byDim[dim].Score != 0 {
			t.Errorf("%s N/A score = %v, want 0", dim, byDim[dim].Score)
		}
	}
	for _, dim := range []string{DimDataType, DimStructure, DimIndex, DimView} {
		if !byDim[dim].IsApplicable() {
			t.Errorf("%s must stay applicable (objects present)", dim)
		}
	}

	// JSON face: N/A dims carry "applicable": false; applicable dims
	// omit the field entirely.
	b, err := json.Marshal(dims)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(b), `"applicable":false`) {
		t.Error("N/A dimensions must serialize \"applicable\":false")
	}
	// Count: exactly 5 N/A dims -> exactly 5 occurrences.
	if got := strings.Count(string(b), `"applicable":false`); got != 5 {
		t.Errorf("applicable=false count = %d, want 5", got)
	}
	if strings.Contains(string(b), `"applicable":true`) {
		t.Error("applicable dims must omit the field (legacy shape)")
	}
}

// TestPGJSONParityPopulated pins the PG parity precondition on the JSON
// face: with every dimension populated, no "applicable" key appears, so
// the payload shape is the pre-batch legacy one.
func TestPGJSONParityPopulated(t *testing.T) {
	dims := NewAssessor().Assess(populatedPGScan())
	b, err := json.Marshal(dims)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(b), "applicable") {
		t.Error("populated PG scan must not emit any applicable key")
	}
}

// TestReportAggregateExcludesNA pins the scoring contract: an N/A
// dimension (score 0, weight e.g. 0.05) must NOT drag the overall score
// — it is excluded from numerator and denominator.
func TestReportAggregateExcludesNA(t *testing.T) {
	dims := []DimensionResult{
		{Dimension: DimDataType, Score: 100, Total: 1},
		{Dimension: DimStructure, Score: 100, Total: 1},
		{Dimension: DimIndex, Score: 100, Total: 1},
		{Dimension: DimView, Score: 100, Total: 1},
		{Dimension: DimFunction, Score: 100, Total: 1},
		{Dimension: DimTrigger, Score: 100, Total: 1},
		{Dimension: DimCustomType, Score: 100, Total: 1},
		{Dimension: DimExtension, Score: 100, Total: 1},
		{Dimension: DimSequence, Score: 100, Total: 1},
	}
	legacy := NewReportGenerator(dims, "").Report()
	if legacy.Score != 100 {
		t.Errorf("all-applicable score = %v, want 100", legacy.Score)
	}

	na := append([]DimensionResult{}, dims...)
	na[8].MarkNotApplicable() // sequence dimension N/A (MySQL shape)
	calibrated := NewReportGenerator(na, "mysql").Report()
	if calibrated.Score != 100 {
		t.Errorf("N/A-excluded score = %v, want 100 (empty dimension must not drag)", calibrated.Score)
	}
}

// TestReportFacesCaliberAndNA pins item 4 (caliber line) and the N/A
// rendering on both report faces. PG renders neither — the parity shape.
func TestReportFacesCaliberAndNA(t *testing.T) {
	res := populatedPGScan()
	dims := NewAssessor().Assess(res)

	// PG faces: no caliber line, no N/A row (byte-identical shape).
	pgHTML := &bytes.Buffer{}
	if err := NewReportGenerator(dims, "").WriteHTML(pgHTML); err != nil {
		t.Fatalf("pg html: %v", err)
	}
	if strings.Contains(pgHTML.String(), "口径") || strings.Contains(pgHTML.String(), "N/A") {
		t.Error("PG HTML must not carry the caliber line or N/A rows (parity)")
	}
	pgTerm := &bytes.Buffer{}
	NewReportGenerator(dims, "").PrintTerminal(pgTerm)
	if strings.Contains(pgTerm.String(), "口径") || strings.Contains(pgTerm.String(), "N/A") {
		t.Error("PG terminal report must not carry the caliber line or N/A rows (parity)")
	}

	// MySQL faces: caliber line present; N/A dims render N/A rows.
	mysqlRes := &ScanResult{
		Tables:  res.Tables,
		Columns: res.Columns,
		Indexes: res.Indexes,
		Views:   res.Views,
	}
	mysqlDims := NewAssessorFor("mysql").Assess(mysqlRes)
	myHTML := &bytes.Buffer{}
	if err := NewReportGenerator(mysqlDims, "mysql").WriteHTML(myHTML); err != nil {
		t.Fatalf("mysql html: %v", err)
	}
	for _, frag := range []string{"源端类型：MySQL（口径：TiDB 目标兼容性）", "N/A 不适用"} {
		if !strings.Contains(myHTML.String(), frag) {
			t.Errorf("mysql HTML missing %q", frag)
		}
	}
	myTerm := &bytes.Buffer{}
	NewReportGenerator(mysqlDims, "mysql").PrintTerminal(myTerm)
	if !strings.Contains(myTerm.String(), "源端类型：MySQL（口径：TiDB 目标兼容性）") {
		t.Error("mysql terminal report missing caliber line")
	}
	if !strings.Contains(myTerm.String(), "不适用（源端无此类对象）") {
		t.Error("mysql terminal report missing N/A row wording")
	}
}
