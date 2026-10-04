package selection

import (
	"context"
	"testing"
	"time"

	"github.com/Tatsubooom/sheepbutler/internal/config"
	"github.com/Tatsubooom/sheepbutler/internal/llm"
	"github.com/Tatsubooom/sheepbutler/internal/model"
)

type fakeStore struct {
	companies map[string]*model.Company
	history   map[string]model.History // 企業ID/メールID
	changes   map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{companies: map[string]*model.Company{}, history: map[string]model.History{}, changes: map[string]string{}}
}

func (f *fakeStore) GetCompany(_ context.Context, id string) (*model.Company, error) {
	if c, ok := f.companies[id]; ok {
		cp := *c
		return &cp, nil
	}
	return nil, nil
}
func (f *fakeStore) SaveCompany(_ context.Context, c *model.Company, h model.History) (bool, error) {
	key := c.ID + "/" + h.MessageID
	if _, ok := f.history[key]; ok {
		return false, nil
	}
	f.history[key] = h
	cp := *c
	f.companies[c.ID] = &cp
	return true, nil
}
func (f *fakeStore) ListCompanies(context.Context) ([]*model.Company, error) {
	var out []*model.Company
	for _, c := range f.companies {
		out = append(out, c)
	}
	return out, nil
}
func (f *fakeStore) SetSelectionChange(_ context.Context, id, text string) error {
	f.changes[id] = text
	return nil
}

var now = time.Date(2026, 10, 10, 12, 0, 0, 0, time.UTC)

func newTracker(st *fakeStore) *Tracker {
	cfg := config.Default()
	cfg.JobHunt.Companies = []config.Company{
		{Name: "株式会社第一志望", Aliases: []string{"ダイイチ"}, Interest: "S"},
		{Name: "興味なし商事", Interest: "none"},
	}
	return &Tracker{Store: st, Config: cfg, Now: func() time.Time { return now }}
}

func mail(id string) *model.Message { return &model.Message{ID: id, ReceivedAt: now.Add(-time.Hour)} }

func sel(company string, ev llm.SelectionEvent) *llm.Extraction {
	return &llm.Extraction{MailKind: llm.KindSelection, Company: company, SelectionEvent: ev}
}

func TestApplyFlow(t *testing.T) {
	st := newFakeStore()
	tr := newTracker(st)
	ctx := context.Background()

	tr.Apply(ctx, mail("m1"), sel("第一志望", llm.EventDocumentRequest))
	tr.Apply(ctx, mail("m2"), sel("株式会社 第一志望", llm.EventResultPass)) // 表記揺れでも同じ企業
	c := st.companies[CompanyID("株式会社第一志望")]
	if c == nil || c.Stage != model.StageInterview || c.Round != 1 || c.AutoCreated {
		t.Fatalf("書類 → 面接（1次）: %+v", c)
	}
	if st.changes["m2"] != "株式会社第一志望 書類・検査 → 面接（1次）" {
		t.Errorf("メールに変化を書き添える: %q", st.changes["m2"])
	}

	// 同じメールをもう一度処理しても二重に進まない（Pub/Sub の再送など）
	tr.Apply(ctx, mail("m2"), sel("第一志望", llm.EventResultPass))
	if st.companies[c.ID].Round != 1 {
		t.Fatal("同じメールで二重に更新しない")
	}

	tr.Apply(ctx, mail("m3"), sel("第一志望", llm.EventResultFail))
	c = st.companies[c.ID]
	if c.Stage != model.StageRejected || !c.Unconfirmed || c.Label() != "不合格？" {
		t.Fatalf("不合格は確認待ち: %+v", c)
	}
}

func TestApplySkips(t *testing.T) {
	st := newFakeStore()
	tr := newTracker(st)
	ctx := context.Background()
	tr.Apply(ctx, mail("a"), nil)
	tr.Apply(ctx, mail("b"), &llm.Extraction{MailKind: llm.KindEvent, Company: "X社"})                                         // 選考イベントなし
	tr.Apply(ctx, mail("c"), &llm.Extraction{MailKind: llm.KindBulk, Company: "X社", SelectionEvent: llm.EventEntryConfirmed}) // 一斉配信
	tr.Apply(ctx, mail("d"), sel("興味なし商事", llm.EventInterviewScheduling))                                                     // 興味なし
	if len(st.companies) != 0 {
		t.Fatalf("選考に関係ないメールでは何もしない: %v", st.companies)
	}
}

func TestApplyAutoCreatesUnknownCompany(t *testing.T) {
	st := newFakeStore()
	newTracker(st).Apply(context.Background(), mail("a"), sel("知らない株式会社", llm.EventEntryConfirmed))
	c := st.companies[CompanyID("知らない株式会社")]
	if c == nil || !c.AutoCreated || c.Stage != model.StageEntry {
		t.Fatalf("%+v", c)
	}
}

func TestOverview(t *testing.T) {
	st := newFakeStore()
	st.companies = map[string]*model.Company{
		"a": {ID: "a", Name: "A社", Stage: model.StageInterview, Round: 2, LastContactAt: now.Add(-2 * 24 * time.Hour), ChangedAt: now.Add(-time.Hour)},
		"b": {ID: "b", Name: "B社", Stage: model.StageDocuments, LastContactAt: now.Add(-15 * 24 * time.Hour)},
		"c": {ID: "c", Name: "C社", Stage: model.StageRejected, Unconfirmed: true, ChangedAt: now.Add(-time.Hour)},
		"d": {ID: "d", Name: "D社", Stage: model.StageInterested},
	}
	o, err := newTracker(st).Overview(context.Background(), now.Add(-12*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(o.Active) != 2 || o.Active[0].ID != "a" {
		t.Errorf("選考中は段階の進んだ順: %v", names(o.Active))
	}
	if len(o.Changed) != 2 || len(o.Review) != 1 || o.Review[0].ID != "c" {
		t.Errorf("changed=%v review=%v", names(o.Changed), names(o.Review))
	}
	if len(o.Stale) != 1 || o.Stale[0].ID != "b" {
		t.Errorf("10日以上連絡のない書類・面接の企業: %v", names(o.Stale))
	}
}

func names(cs []*model.Company) []string {
	var out []string
	for _, c := range cs {
		out = append(out, c.Name)
	}
	return out
}
