package history

import (
	"context"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestJobsLinkedThroughEitherAssociationCountOnce covers a job linked to a
// session only by jobs.session_id, only by job_sessions, and by both.
func TestJobsLinkedThroughEitherAssociationCountOnce(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	createSearchSession(t, store, "target", "node-a", "server-a", "alice", "target", now.Add(-2*time.Hour), now.Add(time.Hour), []int{0, 1})
	createSearchSession(t, store, "other", "node-a", "server-a", "bob", "other", now.Add(-3*time.Hour), now.Add(-2*time.Hour), []int{2})
	insertSearchJob(t, store, "node-a", "job-linked", "other", "authorized_process", "user", []string{"linked"}, 7, []int{1}, now.Add(-100*time.Minute))
	insertSearchJob(t, store, "node-a", "job-primary", "target", "gpuardian_run", "run", []string{"primary"}, 0, []int{0}, now.Add(-90*time.Minute))
	insertSearchJob(t, store, "node-a", "job-both", "target", "gpuardian_run", "run", []string{"both"}, 5, []int{1, 0}, now.Add(-80*time.Minute))
	insertSearchJob(t, store, "node-a", "job-unrelated", "other", "gpuardian_run", "run", []string{"unrelated"}, 0, nil, now.Add(-170*time.Minute))
	if _, err := store.DB().Exec(`INSERT INTO job_sessions(node_id,job_id,session_id) VALUES
		('node-a','job-linked','target'),('node-a','job-linked','other'),('node-a','job-both','target')`); err != nil {
		t.Fatal(err)
	}
	for index, scope := range []struct{ id, session string }{
		{"auth-linked", "other"}, {"auth-primary", "target"}, {"auth-both", "target"}, {"auth-unrelated", "other"},
	} {
		if _, err := store.DB().Exec(`INSERT INTO authorization_scopes(node_id,authorization_id,session_id,mode,holder,created_at_ms)
			VALUES('node-a',?,?,'user','alice',?)`, scope.id, scope.session, millis(now.Add(time.Duration(index)*time.Second))); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec(`INSERT INTO authorization_sessions(node_id,authorization_id,session_id) VALUES
		('node-a','auth-linked','target'),('node-a','auth-both','target')`); err != nil {
		t.Fatal(err)
	}
	wantFirst, wantLast := now.Add(-100*time.Minute), now.Add(-79*time.Minute)

	sessions, err := store.ListSessions(ctx, SessionFilter{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	counts := map[string]int64{}
	for _, session := range sessions {
		counts[session.ID] = session.JobCount
	}
	if !reflect.DeepEqual(counts, map[string]int64{"target": 3, "other": 2}) {
		t.Fatalf("listed job counts = %v", counts)
	}

	detail, err := store.GetSession(ctx, "target")
	if err != nil {
		t.Fatal(err)
	}
	if detail.JobCount != 3 || detail.FirstJobAt == nil || !detail.FirstJobAt.Equal(wantFirst) || detail.LastJobAt == nil || !detail.LastJobAt.Equal(wantLast) {
		t.Fatalf("detail job facts = count %d first %v last %v", detail.JobCount, detail.FirstJobAt, detail.LastJobAt)
	}
	var scopes []string
	for _, scope := range detail.Authorizations {
		scopes = append(scopes, scope.ID)
	}
	wantScopes := []string{
		publicAuthorizationScopeID("node-a", "auth-linked"), publicAuthorizationScopeID("node-a", "auth-primary"), publicAuthorizationScopeID("node-a", "auth-both"),
	}
	if !reflect.DeepEqual(scopes, wantScopes) {
		t.Fatalf("authorization scopes = %v, want %v", scopes, wantScopes)
	}

	jobs, err := store.ListJobs(ctx, "target", 10, 0, "")
	if err != nil {
		t.Fatal(err)
	}
	var listed []string
	for _, job := range jobs {
		listed = append(listed, fmt.Sprintf("%s%v", job.Command[0], job.GPUs))
	}
	if want := []string{"linked[1]", "primary[0]", "both[0 1]"}; !reflect.DeepEqual(listed, want) {
		t.Fatalf("jobs = %v, want %v", listed, want)
	}
	var paged []string
	afterMS, afterID := int64(0), ""
	for page := 0; page < 5; page++ {
		jobs, err := store.ListJobs(ctx, "target", 1, afterMS, afterID)
		if err != nil {
			t.Fatal(err)
		}
		if len(jobs) == 0 {
			break
		}
		paged = append(paged, jobs[0].Command[0])
		afterMS, afterID = jobs[0].StartedAt.UnixMilli(), jobs[0].CursorID
	}
	if want := []string{"linked", "primary", "both"}; !reflect.DeepEqual(paged, want) {
		t.Fatalf("paged jobs = %v, want %v", paged, want)
	}

	for _, test := range []struct {
		name   string
		filter SessionFilter
		want   int64
	}{
		{name: "all sessions", filter: SessionFilter{ServerID: "server-a"}, want: 4},
		{name: "owner case-insensitive", filter: SessionFilter{ServerID: "server-a", Owner: "ALICE"}, want: 3},
		{name: "other owner", filter: SessionFilter{Owner: "bob"}, want: 2},
	} {
		summary, err := store.Summary(ctx, test.filter)
		if err != nil {
			t.Fatal(err)
		}
		if summary.Jobs != test.want {
			t.Fatalf("%s summary jobs = %d, want %d", test.name, summary.Jobs, test.want)
		}
	}

	for _, test := range []struct {
		name         string
		rules        []SearchRule
		sort         SearchSort
		wantSessions []string
		wantCounts   []int64
		wantJobs     int64
	}{
		{name: "job only linked through job_sessions", rules: []SearchRule{searchRule("job.exit_code", "equals", 7)},
			sort: SearchSort{Field: "purpose"}, wantSessions: []string{"other", "target"}, wantCounts: []int64{2, 3}, wantJobs: 4},
		{name: "job linked both ways", rules: []SearchRule{searchRule("job.exit_code", "equals", 5)},
			wantSessions: []string{"target"}, wantCounts: []int64{3}, wantJobs: 3},
		{name: "job gpu through job_sessions", rules: []SearchRule{searchRule("job.gpu", "equals", 1)},
			sort: SearchSort{Field: "job_count", Direction: "desc"}, wantSessions: []string{"target", "other"}, wantCounts: []int64{3, 2}, wantJobs: 4},
		{name: "job without gpus", rules: []SearchRule{searchRuleWithoutValue("job.gpu", "is_empty")},
			wantSessions: []string{"other"}, wantCounts: []int64{2}, wantJobs: 2},
		{name: "job command", rules: []SearchRule{searchRule("job.command", "equals", "LINKED")},
			sort: SearchSort{Field: "job_count", Direction: "asc"}, wantSessions: []string{"other", "target"}, wantCounts: []int64{2, 3}, wantJobs: 4},
		{name: "job count rule", rules: []SearchRule{searchRule("job_count", "equals", 3)},
			wantSessions: []string{"target"}, wantCounts: []int64{3}, wantJobs: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			expression := SearchExpression{Groups: []SearchGroup{{Rules: test.rules}}}
			summary, sessions, _, err := store.Search(ctx, expression, test.sort, 10, SearchCursor{})
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			var jobCounts []int64
			for _, session := range sessions {
				ids = append(ids, session.ID)
				jobCounts = append(jobCounts, session.JobCount)
			}
			if !reflect.DeepEqual(ids, test.wantSessions) || !reflect.DeepEqual(jobCounts, test.wantCounts) ||
				summary.Sessions != int64(len(test.wantSessions)) || summary.Jobs != test.wantJobs {
				t.Fatalf("sessions %v counts %v summary sessions %d jobs %d", ids, jobCounts, summary.Sessions, summary.Jobs)
			}
		})
	}
}

func TestTextRulesKeepASCIICaseInsensitiveSemantics(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	for index, owner := range []string{"Alice", "bob", "ÉVA", "éva"} {
		createSearchSession(t, store, fmt.Sprintf("s%d", index), "node", "server", owner, "Purpose "+owner,
			now.Add(-time.Duration(index+1)*time.Hour), now.Add(time.Hour), []int{index})
	}
	for _, test := range []struct {
		name string
		rule SearchRule
		want []string
	}{
		{name: "equals folds ASCII", rule: searchRule("owner", "equals", "ALICE"), want: []string{"Alice"}},
		{name: "equals keeps non-ASCII case", rule: searchRule("owner", "equals", "éva"), want: []string{"éva"}},
		{name: "not equals", rule: searchRule("owner", "not_equals", "ALICE"), want: []string{"bob", "ÉVA", "éva"}},
		{name: "in folds ASCII", rule: searchRule("owner", "in", []string{"BOB", "alice"}), want: []string{"Alice", "bob"}},
		{name: "in lowers operands", rule: searchRule("owner", "in", []string{"ÉVA"}), want: []string{"éva"}},
		{name: "not in", rule: searchRule("owner", "not_in", []string{"bob"}), want: []string{"Alice", "ÉVA", "éva"}},
		{name: "purpose equals", rule: searchRule("purpose", "equals", "purpose BOB"), want: []string{"bob"}},
		{name: "status in", rule: searchRule("status", "in", []string{"ACTIVE"}), want: []string{"Alice", "bob", "ÉVA", "éva"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, sessions, _, err := store.Search(ctx, SearchExpression{Groups: []SearchGroup{{Rules: []SearchRule{test.rule}}}}, SearchSort{}, 10, SearchCursor{})
			if err != nil {
				t.Fatal(err)
			}
			var owners []string
			for _, session := range sessions {
				owners = append(owners, session.Owner)
			}
			if !reflect.DeepEqual(owners, test.want) {
				t.Fatalf("owners = %v, want %v", owners, test.want)
			}
		})
	}
}

func TestGetSessionWithTimelineLimitReturnsNewestRowsAscending(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	start := time.Now().UTC().Truncate(time.Minute).Add(-24 * time.Hour)
	createSearchSession(t, store, "timeline", "node-a", "server-a", "alice", "timeline", start, start.Add(48*time.Hour), []int{0, 1, 2})
	// GPU 3 has rollups without session membership; it still belongs to the timeline.
	minutes := map[int][]int{0: {0, 1, 2, 3, 4, 5, 6, 7, 8, 9}, 1: {5, 6, 7, 8, 9, 10, 11, 12, 13, 14}, 2: {0, 2, 4}, 3: {14}}
	total := 0
	for gpu, offsets := range minutes {
		for _, offset := range offsets {
			if _, err := store.DB().Exec(`INSERT INTO gpu_minute_rollups(session_id,gpu,minute_ms,observed_ms,busy_ms,utilization_integral)
				VALUES('timeline',?,?,?,?,?)`, gpu, millis(start.Add(time.Duration(offset)*time.Minute)), 1000+offset, offset, float64(1000+offset)*float64(gpu)); err != nil {
				t.Fatal(err)
			}
			total++
		}
	}
	full, err := store.GetSessionWithTimelineLimit(ctx, "timeline", total+10)
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Timeline) != total || !sort.SliceIsSorted(full.Timeline, func(left, right int) bool {
		if !full.Timeline[left].Minute.Equal(full.Timeline[right].Minute) {
			return full.Timeline[left].Minute.Before(full.Timeline[right].Minute)
		}
		return full.Timeline[left].GPU < full.Timeline[right].GPU
	}) {
		t.Fatalf("full timeline has %d rows or is not ascending: %+v", len(full.Timeline), full.Timeline)
	}
	if got := full.Timeline[len(full.Timeline)-1]; got.GPU != 3 || got.AverageUtilization == nil || *got.AverageUtilization != 3 {
		t.Fatalf("newest rollup = %+v", got)
	}
	for _, limit := range []int{1, 2, 3, 7, total - 1, total, total + 10, 0, -1} {
		t.Run(fmt.Sprintf("limit %d", limit), func(t *testing.T) {
			session, err := store.GetSessionWithTimelineLimit(ctx, "timeline", limit)
			if err != nil {
				t.Fatal(err)
			}
			keep := total
			if limit > 0 && limit < total {
				keep = limit
			}
			if want := full.Timeline[total-keep:]; !reflect.DeepEqual(session.Timeline, want) {
				t.Fatalf("timeline = %+v, want %+v", session.Timeline, want)
			}
		})
	}
	defaulted, err := store.GetSession(ctx, "timeline")
	if err != nil || !reflect.DeepEqual(defaulted.Timeline, full.Timeline) {
		t.Fatalf("GetSession timeline = %d rows, error = %v", len(defaulted.Timeline), err)
	}
}

func TestGetSessionCapsTimelineAtDefaultLimit(t *testing.T) {
	store := openTestStore(t)
	start := time.Now().UTC().Truncate(time.Minute).Add(-30 * 24 * time.Hour)
	createSearchSession(t, store, "long", "node-a", "server-a", "alice", "long", start, start.Add(29*24*time.Hour), []int{0})
	extra := 5
	if _, err := store.DB().Exec(`WITH RECURSIVE minutes(n) AS (SELECT 0 UNION ALL SELECT n+1 FROM minutes WHERE n+1<?)
		INSERT INTO gpu_minute_rollups(session_id,gpu,minute_ms,observed_ms) SELECT 'long',0,?+n*60000,1000 FROM minutes`,
		DefaultSessionTimelineLimit+extra, millis(start)); err != nil {
		t.Fatal(err)
	}
	session, err := store.GetSession(context.Background(), "long")
	if err != nil {
		t.Fatal(err)
	}
	if len(session.Timeline) != DefaultSessionTimelineLimit || !session.Timeline[0].Minute.Equal(start.Add(time.Duration(extra)*time.Minute)) {
		t.Fatalf("timeline has %d rows starting %v", len(session.Timeline), session.Timeline[0].Minute)
	}
}

func TestSummaryLimitsNodeWideMetricsToRequestedWindow(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Minute)
	from, to := now.Add(-2*time.Hour), now.Add(-time.Hour)
	createSearchSession(t, store, "window", "node-a", "server-a", "alice", "window", from, to, []int{0})
	for _, rollup := range []struct {
		minute      time.Time
		busy        int64
		utilization float64
	}{
		{minute: from.Add(-time.Minute), busy: 0, utilization: 0},
		{minute: from, busy: 60_000, utilization: 40},
		{minute: to.Add(-time.Minute), busy: 60_000, utilization: 60},
		{minute: to, busy: 0, utilization: 0},
	} {
		if _, err := store.DB().Exec(`INSERT INTO node_gpu_minute_rollups(node_id,gpu,minute_ms,observed_ms,busy_ms,utilization_integral,valid_samples)
			VALUES('node-a',0,?,60000,?,?,1)`, millis(rollup.minute), rollup.busy, rollup.utilization*60_000); err != nil {
			t.Fatal(err)
		}
	}
	for _, test := range []struct {
		name      string
		filter    SessionFilter
		busyHours float64
		ratio     float64
		average   float64
	}{
		{name: "bounded", filter: SessionFilter{ServerID: "server-a", From: &from, To: &to}, busyHours: 2.0 / 60, ratio: 1, average: 50},
		{name: "from only", filter: SessionFilter{ServerID: "server-a", From: &from}, busyHours: 2.0 / 60, ratio: 2.0 / 3, average: 100.0 / 3},
		{name: "unbounded", filter: SessionFilter{ServerID: "server-a", Owner: "alice"}, busyHours: 2.0 / 60, ratio: 0.5, average: 25},
	} {
		summary, err := store.Summary(ctx, test.filter)
		if err != nil {
			t.Fatal(err)
		}
		if math.Abs(summary.BusyGPUHours-test.busyHours) > 1e-9 || math.Abs(summary.BusyRatio-test.ratio) > 1e-9 ||
			summary.AverageUtilization == nil || math.Abs(*summary.AverageUtilization-test.average) > 1e-9 {
			t.Fatalf("%s summary = %+v", test.name, summary)
		}
	}
}

// TestHotQueriesUseIndexes guards the query plans of the rewritten statements:
// none may scan a large table or fall back to an automatic index.
func TestHotQueriesUseIndexes(t *testing.T) {
	store := openTestStore(t)
	compile := func(rules ...SearchRule) (string, []any) {
		predicate, args, err := compileSearchExpression(SearchExpression{Groups: []SearchGroup{{Rules: rules}}})
		if err != nil {
			t.Fatal(err)
		}
		return predicate, args
	}
	factsArgs := []any{int64(1), int64(2), int64(3)}
	jobRules, jobArgs := compile(searchRule("job.source", "equals", "x"), searchRule("job.gpu", "equals", 1),
		searchRule("job.command", "contains", "x"), searchRuleWithoutValue("job.gpu", "is_empty"), searchRule("job.exit_code", "not_equals", 1))
	ownerRule, ownerArgs := compile(searchRule("owner", "in", []string{"a", "b"}))
	purposeRule, purposeArgs := compile(searchRule("purpose", "equals", "x"))
	jobCountRule, jobCountArgs := compile(searchRule("job_count", "gt", 1))
	jobCountSort, err := compileSearchSort(SearchSort{Field: "job_count"})
	if err != nil {
		t.Fatal(err)
	}
	purposeSort, err := compileSearchSort(SearchSort{Field: "purpose"})
	if err != nil {
		t.Fatal(err)
	}
	withArgs := func(groups ...[]any) []any {
		var out []any
		for _, group := range groups {
			out = append(out, group...)
		}
		return out
	}
	for _, test := range []struct {
		name  string
		query string
		args  []any
		want  []string
	}{
		{name: "reconcile open sessions", query: openReservationSessionsSQL, args: []any{"server", 1}, want: []string{"sessions_open_reservation_idx"}},
		{name: "prune events", query: retentionDeleteSQL("ingested_event_ids", "ingested_at_ms"), args: []any{1, 1}, want: []string{"ingested_event_ids_time_idx"}},
		{name: "prune session rollups", query: retentionDeleteSQL("gpu_minute_rollups", "minute_ms"), args: []any{1, 1}, want: []string{"gpu_rollups_time_idx"}},
		{name: "prune node rollups", query: retentionDeleteSQL("node_gpu_minute_rollups", "minute_ms"), args: []any{1, 1}, want: []string{"node_gpu_rollups_time_idx"}},
		{name: "session job stats", query: sessionJobStatsSQL(2), args: []any{"a", "b"}, want: []string{"jobs_session_idx", "job_sessions_session_idx"}},
		{name: "list jobs", query: listJobsSQL(true), args: []any{"s", "s", 1, 1, "j", 10}, want: []string{"jobs_session_idx", "job_sessions_session_idx"}},
		{name: "job gpus", query: jobGPUsSQL(2), args: []any{"n", "a", "n", "b"}, want: []string{"sqlite_autoindex_job_gpus_1"}},
		{name: "authorization scopes", query: authorizationScopesSQL, args: []any{"s", "s"}, want: []string{"authorization_scopes_session_idx", "authorization_sessions_session_idx"}},
		{name: "summary jobs", query: summaryJobCountSQL(" WHERE r.provisioning=0 AND r.kind='reservation' AND r.revoked_at_ms IS NOT NULL"), want: []string{"jobs_session_idx", "job_sessions_session_idx"}},
		{name: "search summary job rules", query: searchSummarySQL(jobRules, false), args: withArgs(factsArgs, jobArgs), want: []string{"jobs_session_idx", "job_sessions_session_idx", "sqlite_autoindex_job_gpus_1"}},
		{name: "search summary owner", query: searchSummarySQL(ownerRule, false), args: withArgs(factsArgs, ownerArgs), want: []string{"sessions_owner_idx"}},
		{name: "search page purpose", query: searchAdvancedPageSQL(sessionFactsCTE(false), purposeRule, purposeSort), args: withArgs(factsArgs, purposeArgs, []any{10}), want: []string{"sessions_purpose_idx"}},
		{name: "search page job count", query: searchAdvancedPageSQL(sessionFactsCTE(true), jobCountRule, jobCountSort), args: withArgs(factsArgs, jobCountArgs, []any{10}), want: []string{"jobs_session_idx", "job_sessions_session_idx"}},
		{name: "timeline gpus", query: timelineGPUsSQL, args: []any{"s", "s"}, want: []string{"COVERING INDEX sqlite_autoindex_gpu_minute_rollups_1 (session_id=? AND gpu>?)"}},
		{name: "timeline minutes", query: timelineMinutesSQL, args: []any{"s", 0, 10}, want: []string{"COVERING INDEX sqlite_autoindex_gpu_minute_rollups_1 (session_id=? AND gpu=?)"}},
		{name: "timeline rows", query: timelineRowsSQL, args: []any{"s", 0, 1}, want: []string{"sqlite_autoindex_gpu_minute_rollups_1 (session_id=? AND gpu=? AND minute_ms>?)"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			plan := explainQueryPlan(t, store, test.query, test.args...)
			for _, line := range plan {
				for _, table := range []string{"jobs", "j", "job_sessions", "js", "job_gpus", "jg", "authorization_scopes", "authorization_sessions",
					"gpu_minute_rollups", "node_gpu_minute_rollups", "ingested_event_ids"} {
					if line == "SCAN "+table || strings.HasPrefix(line, "SCAN "+table+" ") {
						t.Fatalf("plan scans %s:\n%s", table, strings.Join(plan, "\n"))
					}
				}
				if strings.Contains(line, "AUTOMATIC") {
					t.Fatalf("plan builds an automatic index:\n%s", strings.Join(plan, "\n"))
				}
			}
			joined := strings.Join(plan, "\n")
			for _, want := range test.want {
				if !strings.Contains(joined, want) {
					t.Fatalf("plan does not use %s:\n%s", want, joined)
				}
			}
		})
	}
}

func TestIndexMigrationUpgradesV9Database(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history.db")
	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	indexes := []string{"ingested_event_ids_time_idx", "gpu_rollups_time_idx", "authorization_scopes_session_idx", "sessions_open_reservation_idx"}
	for _, index := range indexes {
		if _, err := store.DB().Exec("DROP INDEX " + index); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.DB().Exec("DELETE FROM schema_migrations WHERE version=10"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	for _, index := range indexes {
		var found int
		if err := store.DB().QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name=?", index).Scan(&found); err != nil || found != 1 {
			t.Fatalf("index %s after upgrade: found=%d err=%v", index, found, err)
		}
	}
	var version int
	if err := store.DB().QueryRow("SELECT MAX(version) FROM schema_migrations").Scan(&version); err != nil || version != 10 {
		t.Fatalf("schema version = %d, err = %v", version, err)
	}
}

func explainQueryPlan(t *testing.T, store *Store, query string, args ...any) []string {
	t.Helper()
	rows, err := store.DB().Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var plan []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		plan = append(plan, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return plan
}
