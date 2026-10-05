package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/agent-mem/agent-mem/internal/graph/jobs"
	"github.com/rs/zerolog"
)

func TestIndexArtifactNode_Extracted(t *testing.T) {
	pool, rec := decisionIndexTestDB(t)
	ctx := context.Background()
	deps := Deps{DB: pool, Gemini: rec, Logger: zerolog.Nop(), MachineID: "test"}
	const id = "jira:PAY-2333"
	if _, err := pool.Exec(ctx, `INSERT INTO graph.nodes(id,type,natural_key,title,body,machine_id) VALUES($1,'jira',$1,'Fix refunds','Background\n\nReturn HTTP 409','test')`, id); err != nil {
		t.Fatal(err)
	}
	type row struct {
		summary, kind, embedding string
		ids                      []string
	}
	read := func() row {
		var r row
		if err := pool.QueryRow(ctx, `SELECT summary,summary_kind,embedding::text,identifiers FROM graph.artifact_index WHERE node_id=$1`, id).Scan(&r.summary, &r.kind, &r.embedding, &r.ids); err != nil {
			t.Fatal(err)
		}
		return r
	}
	if err := indexArtifactNode(ctx, deps, id, true, true); err != nil {
		t.Fatal(err)
	}
	direct := read()
	if _, err := pool.Exec(ctx, `DELETE FROM graph.artifact_index WHERE node_id=$1`, id); err != nil {
		t.Fatal(err)
	}
	p, _ := json.Marshal(indexArtifactPayload{NodeID: id, Force: true, SkipJudging: true})
	h := NewIndexArtifactHandler(deps).Handler
	if err := h(ctx, p); err != nil {
		t.Fatal(err)
	}
	queued := read()
	if !reflect.DeepEqual(direct, queued) {
		t.Fatalf("direct=%+v queued=%+v", direct, queued)
	}
	for _, p := range [][]byte{[]byte("invalid"), []byte(`{}`)} {
		if err := h(ctx, p); !errors.Is(err, jobs.ErrFatal) {
			t.Fatalf("payload %q error=%v", p, err)
		}
	}
	before := rec.embedCalls.Load()
	if err := indexArtifactNode(ctx, deps, id, false, true); err != nil {
		t.Fatal(err)
	}
	if rec.embedCalls.Load() != before {
		t.Fatal("fresh row embedded again")
	}
	rec.embedResult = func() ([]float32, error) { return nil, errors.New("provider unavailable") }
	err := indexArtifactNode(ctx, deps, id, true, true)
	var embedErr *artifactEmbedError
	if !errors.As(err, &embedErr) || !errors.Is(err, jobs.ErrTransient) {
		t.Fatalf("embed classification=%v", err)
	}
	if got := read(); !reflect.DeepEqual(got, direct) {
		t.Fatal("failed embed changed index")
	}
}

func TestIndexArtifactNode_SlackBlankParagraphUsesCachedSummary(t *testing.T) {
	pool, rec := decisionIndexTestDB(t)
	seedDecisionIndexThread(t, pool)
	ctx := context.Background()
	const id = "slack:CDKW:900.000001"
	if _, err := pool.Exec(ctx, `UPDATE graph.nodes SET body=$2 WHERE id=$1`, id, "\n\nActual message mentions PAY-2333"); err != nil {
		t.Fatal(err)
	}
	runDecisionIndex(t, pool, rec, id)
	var summary, kind, decisions string
	var ids []string
	if err := pool.QueryRow(ctx, `SELECT summary,summary_kind,decisions_text,identifiers FROM graph.artifact_index WHERE node_id=$1`, id).Scan(&summary, &kind, &decisions, &ids); err != nil {
		t.Fatal(err)
	}
	if summary != "WOMBATTOPIC flow\n\nTeam discussed refunds." || kind != "thread_summary" ||
		decisions != "- Use the QUOKKAPAY ledger for partial refunds" || !containsIdentifier(ids, "PAY-2333") {
		t.Fatalf("cached Slack summary lost: summary=%q kind=%q decisions=%q ids=%v", summary, kind, decisions, ids)
	}
	if rec.embedCalls.Load() != 1 || rec.inputs[0] != summary+indexDecisionsMarker+decisions {
		t.Fatalf("cached summary embedding=%v", rec.inputs)
	}
}
