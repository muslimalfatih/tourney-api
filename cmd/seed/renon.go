// Renon Cup 2026 — the deterministic development dataset (Refactor Phase 2B).
//
// Runs as part of `make seed`, after the admin/org bootstrap in main.go. Safe
// to run any number of times: every entity is looked up by a stable natural
// key before anything is created — tournament by slug, divisions by
// (name, category), pairs by display_name, round-robin fixtures by their
// unordered participant pair, schedule slots by (court, start time) — and
// completed results are never re-submitted. Reruns report "existing" instead
// of duplicating.
//
// All writes go through the real service layer (participant/draw/match/
// schedule services), so the seed exercises the same validation, transactions
// and side effects (winner advancement, group→knockout resolution, SSE topic
// gating) as the product itself. If required schema is missing, the first
// service call fails loudly with its underlying error.
//
// The dataset is sized to exercise the whole current product:
//
//	D1 Men's Doubles Beginner   single_elim    8 pairs, R1 built, 2 completed
//	                                           (winners advanced), 1 live, 1 up
//	D2 Women's Doubles Beg++    round_robin    5 pairs, full 10-fixture RR,
//	                                           3 completed → standings
//	D3 Mixed Doubles Beginner   group_knockout 8 pairs, 2 groups of 4, all 12
//	                                           group matches completed →
//	                                           placements resolved into semis;
//	                                           final stays winner-of-match /
//	                                           unresolved
//	D4 Men's Doubles Intermediate round_robin  6 pairs, no fixtures (empty
//	                                           state)
//	D5 Men's Doubles Open       single_elim    6 pairs, built from 3 explicit
//	                                           pairings → padded with a bye
//	Schedule: 2 courts, slots across Sep 19–20 (two derived match days)
package main

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/muslimalfatih/laga-api/internal/draw"
	"github.com/muslimalfatih/laga-api/internal/event"
	"github.com/muslimalfatih/laga-api/internal/match"
	"github.com/muslimalfatih/laga-api/internal/participant"
	"github.com/muslimalfatih/laga-api/internal/schedule"
	"github.com/muslimalfatih/laga-api/internal/tournament"
)

const renonSlug = "renon-cup-2026"

type divisionSpec struct {
	name, category, gender, format, display string
	pairs                                   []string
}

var renonDivisions = []divisionSpec{
	{"Men's Doubles", "Beginner", "men", "single_elim", "Men's Doubles Beginner", []string{
		"Wayan Suryadi / Kadek Arimbawa", "Made Pranata / Nyoman Wirawan",
		"Bayu Prasetyo / Eko Nugroho", "Agus Setiawan / Hendra Gunawan",
		"Dedi Hartono / Rizky Ramadhan", "Fajar Nugraha / Gede Wirautama",
		"Komang Adi Putra / Putu Darmawan", "Yoga Firmansyah / Arya Wicaksono",
	}},
	{"Women's Doubles", "Beginner++", "women", "round_robin", "Women's Doubles Beginner++", []string{
		"Ayu Kirana / Dewi Larasati", "Ni Kadek Wulan / Ratih Anggraini",
		"Intan Permata / Citra Maharani", "Yulia Safitri / Diah Puspita",
		"Emma Clarke / Chloe Bennett",
	}},
	{"Mixed Doubles", "Beginner", "mixed", "group_knockout", "Mixed Doubles Beginner", []string{
		"Jack Mitchell / Olivia Turner", "Ryan O'Brien / Priya Nair",
		"Liam Foster / Ananya Iyer", "Ethan Coleman / Simran Gill",
		"Arjun Mehta / Sofia Rossi", "Rohan Kapoor / Marie Dubois",
		"Vikram Chauhan / Isabella Cruz", "Aditya Bhatt / Aiko Suzuki",
	}},
	{"Men's Doubles", "Intermediate", "men", "round_robin", "Men's Doubles Intermediate", []string{
		"Haruto Sato / Ren Kobayashi", "Kenta Yamamoto / Min-jun Kim",
		"Seo-jun Park / Lukas Weber", "Erik Johansson / Rafael Costa",
		"Miguel Santos / Andi Wirasena", "Bagus Kristanto / Dimas Aprilianto",
	}},
	{"Men's Doubles", "Open", "men", "single_elim", "Men's Doubles Open", []string{
		"Putu Mahendra / Ketut Swastika", "Daniel Wright / Tom Becker",
		"Ravi Shankar / Dev Patel", "Marco Bianchi / Luca Romano",
		"Gede Artha / Wayan Dipta", "Chris Tanaka / Ben Osaka",
	}},
}

// tally is the created-vs-existing report the seed prints per section.
type tally struct{ created, existing int }

func (t *tally) hit(created bool) {
	if created {
		t.created++
	} else {
		t.existing++
	}
}

type renonSeeder struct {
	pool        *pgxpool.Pool
	orgID       uuid.UUID
	organizerID uuid.UUID

	tournaments  *tournament.Service
	events       *event.Service
	participants *participant.Service
	draws        *draw.Service
	matches      *match.Service
	schedules    *schedule.Service

	report map[string]*tally
}

func seedRenonCup(ctx context.Context, pool *pgxpool.Pool) error {
	s := &renonSeeder{
		pool:         pool,
		tournaments:  tournament.NewService(pool),
		events:       event.NewService(pool),
		participants: participant.NewService(pool),
		draws:        draw.NewService(pool),
		matches:      match.NewService(pool),
		schedules:    schedule.NewService(pool),
		report:       map[string]*tally{},
	}
	for _, k := range []string{"tournament", "divisions", "pairs", "fixtures", "results", "courts", "slots"} {
		s.report[k] = &tally{}
	}

	// Org + organizer come from the bootstrap seed that just ran.
	err := pool.QueryRow(ctx, `SELECT id FROM organizations WHERE slug = 'laga-demo'`).Scan(&s.orgID)
	if err != nil {
		return fmt.Errorf("renon seed: demo org missing (run the base seed first): %w", err)
	}
	if err := pool.QueryRow(ctx,
		`SELECT id FROM users WHERE org_id = $1 AND role = 'organizer' ORDER BY created_at LIMIT 1`,
		s.orgID).Scan(&s.organizerID); err != nil {
		return fmt.Errorf("renon seed: demo organizer missing: %w", err)
	}

	tid, err := s.ensureTournament(ctx)
	if err != nil {
		return err
	}
	divIDs := map[string]uuid.UUID{}  // display name -> event id
	pairIDs := map[string]uuid.UUID{} // "display|pair" -> participant id
	for _, spec := range renonDivisions {
		evID, err := s.ensureDivision(ctx, tid, spec)
		if err != nil {
			return err
		}
		divIDs[spec.display] = evID
		if err := s.ensurePairs(ctx, evID, spec, pairIDs); err != nil {
			return err
		}
	}

	if err := s.seedMensBeginner(ctx, divIDs["Men's Doubles Beginner"], pairIDs); err != nil {
		return err
	}
	if err := s.seedWomensRR(ctx, divIDs["Women's Doubles Beginner++"], pairIDs); err != nil {
		return err
	}
	if err := s.seedMixedGroupKnockout(ctx, divIDs["Mixed Doubles Beginner"], pairIDs); err != nil {
		return err
	}
	if err := s.seedMensOpenBye(ctx, divIDs["Men's Doubles Open"], pairIDs); err != nil {
		return err
	}
	if err := s.ensureSchedule(ctx, tid, divIDs); err != nil {
		return err
	}
	if err := s.ensurePublished(ctx, tid); err != nil {
		return err
	}

	for _, k := range []string{"tournament", "divisions", "pairs", "fixtures", "results", "courts", "slots"} {
		t := s.report[k]
		slog.Info("renon seed", slog.String("section", k),
			slog.Int("created", t.created), slog.Int("existing", t.existing))
	}
	return nil
}

func (s *renonSeeder) ensureTournament(ctx context.Context) (uuid.UUID, error) {
	var id uuid.UUID
	err := s.pool.QueryRow(ctx, `SELECT id FROM tournaments WHERE slug = $1`, renonSlug).Scan(&id)
	if err == nil {
		s.report["tournament"].hit(false)
		return id, nil
	}
	if err != pgx.ErrNoRows {
		return uuid.Nil, fmt.Errorf("renon seed: tournament lookup: %w", err)
	}
	t, err := s.tournaments.Create(ctx, s.orgID, tournament.CreateTournamentRequest{
		Name:        "Renon Cup 2026",
		Slug:        renonSlug,
		Sport:       "tennis",
		Description: "A community doubles cup at the Renon courts in Denpasar, open to beginner and intermediate pairs across men's, women's and mixed draws.",
		Location:    "Lapangan Renon, Denpasar, Bali",
		StartsOn:    "2026-09-19",
		EndsOn:      "2026-09-21",
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("renon seed: create tournament: %w", err)
	}
	s.report["tournament"].hit(true)
	return t.ID, nil
}

func (s *renonSeeder) ensureDivision(ctx context.Context, tid uuid.UUID, spec divisionSpec) (uuid.UUID, error) {
	existing, err := s.events.ListForTournament(ctx, tid, &s.orgID)
	if err != nil {
		return uuid.Nil, fmt.Errorf("renon seed: list divisions: %w", err)
	}
	for _, ev := range existing {
		cat := ""
		if ev.Category != nil {
			cat = *ev.Category
		}
		if ev.Name == spec.name && cat == spec.category {
			s.report["divisions"].hit(false)
			return ev.ID, nil
		}
	}
	ev, err := s.events.Create(ctx, tid, &s.orgID, event.CreateEventRequest{
		Name: spec.name, Discipline: "doubles", Format: spec.format, Category: spec.category,
	})
	if err != nil {
		return uuid.Nil, fmt.Errorf("renon seed: create division %s/%s: %w", spec.name, spec.category, err)
	}
	gender, display := spec.gender, spec.display
	if _, err := s.events.UpdatePublicSettings(ctx, ev.ID, &s.orgID, event.UpdateInput{
		Gender: &gender, PublicDisplayName: &display,
	}); err != nil {
		return uuid.Nil, fmt.Errorf("renon seed: division public settings: %w", err)
	}
	s.report["divisions"].hit(true)
	return ev.ID, nil
}

// ensurePairs get-or-creates each pair by display_name and records its
// participant id under "<division display>|<pair name>". The first two pairs
// of every division carry seeds 1 and 2.
func (s *renonSeeder) ensurePairs(ctx context.Context, evID uuid.UUID, spec divisionSpec, out map[string]uuid.UUID) error {
	existing, err := s.participants.List(ctx, evID, &s.orgID)
	if err != nil {
		return fmt.Errorf("renon seed: list pairs: %w", err)
	}
	byName := map[string]uuid.UUID{}
	for _, p := range existing {
		byName[p.DisplayName] = p.ID
	}
	for i, name := range spec.pairs {
		if id, ok := byName[name]; ok {
			out[spec.display+"|"+name] = id
			s.report["pairs"].hit(false)
			continue
		}
		var seed *int
		if i < 2 {
			v := i + 1
			seed = &v
		}
		p, err := s.participants.Add(ctx, evID, &s.orgID, participant.AddParticipantRequest{
			DisplayName: name, Seed: seed,
		})
		if err != nil {
			return fmt.Errorf("renon seed: add pair %q: %w", name, err)
		}
		out[spec.display+"|"+name] = p.ID
		s.report["pairs"].hit(true)
	}
	return nil
}

func (s *renonSeeder) pair(pairIDs map[string]uuid.UUID, display string, spec divisionSpec, idx int) uuid.UUID {
	return pairIDs[display+"|"+spec.pairs[idx]]
}

// completeMatch submits a deterministic completed result, orienting the set
// scores so the intended winner wins regardless of which slot they landed in.
// winnerSets are from the WINNER's perspective, e.g. {{6,3},{6,4}}.
func (s *renonSeeder) completeMatch(ctx context.Context, m match.Match, winnerID uuid.UUID, winnerSets [][2]int) (bool, error) {
	if m.Status == "completed" {
		s.report["results"].hit(false)
		return false, nil
	}
	winnerSlot := 0
	for _, sv := range m.Participants {
		if sv.ParticipantID != nil && *sv.ParticipantID == winnerID {
			winnerSlot = sv.Slot
		}
	}
	if winnerSlot == 0 {
		return false, fmt.Errorf("renon seed: winner %s not in match %d", winnerID, m.MatchNo)
	}
	sets := make([]match.SetScore, 0, len(winnerSets))
	for i, ws := range winnerSets {
		p1, p2 := ws[0], ws[1]
		if winnerSlot == 2 {
			p1, p2 = p2, p1
		}
		sets = append(sets, match.SetScore{SetNumber: i + 1, P1Games: p1, P2Games: p2})
	}
	if _, _, err := s.matches.SubmitScore(ctx, m.ID, s.organizerID, &s.orgID, match.ScoreRequest{Sets: sets, Completion: match.CompletionNormal}); err != nil {
		return false, fmt.Errorf("renon seed: complete match %d: %w", m.MatchNo, err)
	}
	s.report["results"].hit(true)
	return true, nil
}

// D1: single_elim, 8 pairs. Bracket built once from seeds (1v8, 4v5, 3v6,
// 2v7); R1 matches 1-2 completed (winners advance into the semis —
// winner-of-match slots resolving), match 3 live with a partial score, match 4
// still to play.
func (s *renonSeeder) seedMensBeginner(ctx context.Context, evID uuid.UUID, pairIDs map[string]uuid.UUID) error {
	spec := renonDivisions[0]
	ms, err := s.matches.ListForEvent(ctx, evID)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		p := func(i int) *uuid.UUID { id := s.pair(pairIDs, spec.display, spec, i); return &id }
		if _, err := s.draws.Build(ctx, evID, &s.orgID, draw.BuildInput{Pairs: []draw.Pair{
			{A: p(0), B: p(7)}, {A: p(3), B: p(4)}, {A: p(2), B: p(5)}, {A: p(1), B: p(6)},
		}}); err != nil {
			return fmt.Errorf("renon seed: build D1 bracket: %w", err)
		}
		s.report["fixtures"].created += 7
		ms, err = s.matches.ListForEvent(ctx, evID)
		if err != nil {
			return err
		}
	} else {
		s.report["fixtures"].existing += len(ms)
	}

	byNo := map[int]match.Match{}
	for _, m := range ms {
		byNo[m.MatchNo] = m
	}
	// The winner of each completed R1 match is whoever holds slot 1 — chosen
	// from the match itself, not from the spec's pair order, so the seed also
	// adopts a pre-existing bracket whose R1 arrangement differs (the original
	// dev bracket ordered unseeded pairs alphabetically). Deterministic either
	// way: slot assignments never change after a build.
	slot1 := func(m match.Match) (uuid.UUID, bool) {
		for _, sv := range m.Participants {
			if sv.Slot == 1 && sv.ParticipantID != nil {
				return *sv.ParticipantID, true
			}
		}
		return uuid.Nil, false
	}
	if m, ok := byNo[1]; ok {
		if w, has := slot1(m); has {
			if _, err := s.completeMatch(ctx, m, w, [][2]int{{6, 3}, {6, 4}}); err != nil {
				return err
			}
		}
	}
	if m, ok := byNo[2]; ok {
		if w, has := slot1(m); has {
			if _, err := s.completeMatch(ctx, m, w, [][2]int{{7, 5}, {6, 2}}); err != nil {
				return err
			}
		}
	}
	if m, ok := byNo[3]; ok && m.Status != "completed" && m.Status != "live" {
		// Partial score, not complete → service marks it live.
		if _, _, err := s.matches.SubmitScore(ctx, m.ID, s.organizerID, &s.orgID, match.ScoreRequest{
			Sets: []match.SetScore{{SetNumber: 1, P1Games: 6, P2Games: 4}}, Completion: match.CompletionIncomplete,
		}); err != nil {
			return fmt.Errorf("renon seed: mark D1 match 3 live: %w", err)
		}
		s.report["results"].hit(true)
	}
	return nil
}

// D2: full round robin over 5 pairs = 10 fixtures, keyed by unordered pair.
// Three deterministic results feed the standings; the rest stay pending.
func (s *renonSeeder) seedWomensRR(ctx context.Context, evID uuid.UUID, pairIDs map[string]uuid.UUID) error {
	spec := renonDivisions[1]
	ms, err := s.matches.ListForEvent(ctx, evID)
	if err != nil {
		return err
	}
	key := func(a, b uuid.UUID) string {
		x, y := a.String(), b.String()
		if x > y {
			x, y = y, x
		}
		return x + "|" + y
	}
	have := map[string]match.Match{}
	for _, m := range ms {
		var ids []uuid.UUID
		for _, sv := range m.Participants {
			if sv.ParticipantID != nil {
				ids = append(ids, *sv.ParticipantID)
			}
		}
		if len(ids) == 2 {
			have[key(ids[0], ids[1])] = m
		}
	}
	n := len(spec.pairs)
	for i := 0; i < n; i++ {
		for j := i + 1; j < n; j++ {
			a, b := s.pair(pairIDs, spec.display, spec, i), s.pair(pairIDs, spec.display, spec, j)
			if _, ok := have[key(a, b)]; ok {
				s.report["fixtures"].hit(false)
				continue
			}
			if _, _, err := s.draws.AddManualMatch(ctx, evID, &s.orgID, s.organizerID, a, b, false); err != nil {
				return fmt.Errorf("renon seed: add RR fixture %d-%d: %w", i, j, err)
			}
			s.report["fixtures"].hit(true)
		}
	}

	// Re-list so freshly created fixtures are completable, then post the three
	// deterministic results: 0 beats 3, 0 beats 2, 2 beats 3. Pairings 0-1 and
	// 2-4 are deliberately NOT completed — they carry the Sep 20 schedule
	// slots, and a completed match on a future slot would read as broken.
	ms, err = s.matches.ListForEvent(ctx, evID)
	if err != nil {
		return err
	}
	have = map[string]match.Match{}
	for _, m := range ms {
		var ids []uuid.UUID
		for _, sv := range m.Participants {
			if sv.ParticipantID != nil {
				ids = append(ids, *sv.ParticipantID)
			}
		}
		if len(ids) == 2 {
			have[key(ids[0], ids[1])] = m
		}
	}
	results := []struct {
		wi, li int
		sets   [][2]int
	}{
		{0, 3, [][2]int{{6, 2}, {6, 3}}},
		{0, 2, [][2]int{{6, 4}, {7, 5}}},
		{2, 3, [][2]int{{7, 5}, {6, 4}}},
	}
	for _, r := range results {
		w, l := s.pair(pairIDs, spec.display, spec, r.wi), s.pair(pairIDs, spec.display, spec, r.li)
		m, ok := have[key(w, l)]
		if !ok {
			return fmt.Errorf("renon seed: RR fixture %d-%d missing after ensure", r.wi, r.li)
		}
		if _, err := s.completeMatch(ctx, m, w, r.sets); err != nil {
			return err
		}
	}
	return nil
}

// D3: group_knockout. Two groups of four; every group match gets a
// deterministic result so the group placements resolve into the semifinals
// (group_placement slots → resolved), while the final's winner-of-semi slots
// stay unresolved — the "empty until played" state, distinct from a bye.
func (s *renonSeeder) seedMixedGroupKnockout(ctx context.Context, evID uuid.UUID, pairIDs map[string]uuid.UUID) error {
	spec := renonDivisions[2]
	ms, err := s.matches.ListForEvent(ctx, evID)
	if err != nil {
		return err
	}
	if len(ms) == 0 {
		ids := func(from, to int) []uuid.UUID {
			var out []uuid.UUID
			for i := from; i <= to; i++ {
				out = append(out, s.pair(pairIDs, spec.display, spec, i))
			}
			return out
		}
		if _, err := s.draws.SetGroups(ctx, evID, &s.orgID, []draw.GroupSpec{
			{Name: "Group A", AdvanceCount: 2, TeamIDs: ids(0, 3)},
			{Name: "Group B", AdvanceCount: 2, TeamIDs: ids(4, 7)},
		}); err != nil {
			return fmt.Errorf("renon seed: set D3 groups: %w", err)
		}
		ms, err = s.matches.ListForEvent(ctx, evID)
		if err != nil {
			return err
		}
		s.report["fixtures"].created += len(ms)
	} else {
		s.report["fixtures"].existing += len(ms)
	}

	// Knockout match ids from the read model; everything else is a group match.
	gk, err := s.draws.GetGroupKnockout(ctx, evID)
	if err != nil {
		return err
	}
	knockout := map[uuid.UUID]bool{}
	for _, r := range gk.Knockout {
		for _, m := range r.Matches {
			knockout[m.ID] = true
		}
	}

	// Deterministic round-robin outcome inside each group: the lower index
	// always wins, so placements are index-ordered (0,1 advance from A; 4,5
	// from B). Winner = participant whose spec index is smaller.
	idxOf := map[uuid.UUID]int{}
	for i := range spec.pairs {
		idxOf[s.pair(pairIDs, spec.display, spec, i)] = i
	}
	for _, m := range ms {
		if knockout[m.ID] || m.Status == "completed" || m.Status == "bye" {
			if m.Status == "completed" {
				s.report["results"].hit(false)
			}
			continue
		}
		var ids []uuid.UUID
		for _, sv := range m.Participants {
			if sv.ParticipantID != nil {
				ids = append(ids, *sv.ParticipantID)
			}
		}
		if len(ids) != 2 {
			continue
		}
		winner := ids[0]
		if idxOf[ids[1]] < idxOf[ids[0]] {
			winner = ids[1]
		}
		if _, err := s.completeMatch(ctx, m, winner, [][2]int{{6, 3}, {6, 2}}); err != nil {
			return err
		}
	}

	// The last completed group match auto-resolves the knockout; run resolve
	// once more explicitly so the seed's end state never depends on ordering.
	if _, err := s.draws.ResolveGroups(ctx, evID, &s.orgID); err != nil {
		return fmt.Errorf("renon seed: resolve D3 groups: %w", err)
	}
	return nil
}

// D5: single_elim built from three explicit pairings over six pairs — the
// generator pads three matches up to four, producing a bye, which is the
// state the bye-vs-empty distinction is demonstrated with.
func (s *renonSeeder) seedMensOpenBye(ctx context.Context, evID uuid.UUID, pairIDs map[string]uuid.UUID) error {
	spec := renonDivisions[4]
	ms, err := s.matches.ListForEvent(ctx, evID)
	if err != nil {
		return err
	}
	if len(ms) > 0 {
		s.report["fixtures"].existing += len(ms)
		return nil
	}
	p := func(i int) *uuid.UUID { id := s.pair(pairIDs, spec.display, spec, i); return &id }
	if _, err := s.draws.Build(ctx, evID, &s.orgID, draw.BuildInput{Pairs: []draw.Pair{
		{A: p(0), B: p(5)}, {A: p(2), B: p(3)}, {A: p(1), B: p(4)},
	}}); err != nil {
		return fmt.Errorf("renon seed: build D5 bracket: %w", err)
	}
	ms, err = s.matches.ListForEvent(ctx, evID)
	if err != nil {
		return err
	}
	s.report["fixtures"].created += len(ms)
	byes := 0
	for _, m := range ms {
		if m.Status == "bye" {
			byes++
		}
	}
	if byes == 0 {
		return fmt.Errorf("renon seed: D5 expected a padded bye match, found none")
	}
	return nil
}

// ensureSchedule creates the two courts and a deterministic slot plan across
// two match days (Sep 19 and 20, derived from timestamps). Slots are keyed by
// (court, start time); a slot that already exists is left untouched, whatever
// match it carries.
func (s *renonSeeder) ensureSchedule(ctx context.Context, tid uuid.UUID, divIDs map[string]uuid.UUID) error {
	courts, err := s.schedules.ListCourts(ctx, tid, &s.orgID)
	if err != nil {
		return err
	}
	courtByName := map[string]uuid.UUID{}
	for _, c := range courts {
		courtByName[c.Name] = c.ID
	}
	for i, name := range []string{"Court 1", "Court 2"} {
		if _, ok := courtByName[name]; ok {
			s.report["courts"].hit(false)
			continue
		}
		c, err := s.schedules.CreateCourt(ctx, tid, &s.orgID, schedule.CreateCourtRequest{Name: name, SortOrder: i})
		if err != nil {
			return fmt.Errorf("renon seed: create court %s: %w", name, err)
		}
		courtByName[name] = c.ID
		s.report["courts"].hit(true)
	}

	// Deterministic match lookup: division display name + match_no.
	matchNo := func(display string, no int) *uuid.UUID {
		ms, err := s.matches.ListForEvent(ctx, divIDs[display])
		if err != nil {
			return nil
		}
		for _, m := range ms {
			if m.MatchNo == no {
				id := m.ID
				return &id
			}
		}
		return nil
	}

	type slotSpec struct {
		court, start string
		matchID      *uuid.UUID
	}
	plan := []slotSpec{
		{"Court 1", "2026-09-19T08:00:00Z", matchNo("Men's Doubles Beginner", 1)},
		{"Court 2", "2026-09-19T08:00:00Z", matchNo("Men's Doubles Beginner", 2)},
		{"Court 1", "2026-09-19T09:30:00Z", matchNo("Men's Doubles Beginner", 3)},
		{"Court 2", "2026-09-19T09:30:00Z", matchNo("Men's Doubles Beginner", 4)},
		{"Court 1", "2026-09-19T11:00:00Z", matchNo("Mixed Doubles Beginner", 1)},
		{"Court 2", "2026-09-19T11:00:00Z", matchNo("Mixed Doubles Beginner", 2)},
		{"Court 1", "2026-09-20T08:00:00Z", matchNo("Women's Doubles Beginner++", 15)},
		{"Court 2", "2026-09-20T08:00:00Z", matchNo("Women's Doubles Beginner++", 16)},
	}

	existing, err := s.schedules.ListSlots(ctx, tid, &s.orgID)
	if err != nil {
		return err
	}
	taken := map[string]bool{}
	for _, sl := range existing {
		taken[sl.CourtID.String()+"|"+sl.StartsAt.UTC().Format(time.RFC3339)] = true
	}
	for _, sp := range plan {
		courtID := courtByName[sp.court]
		if taken[courtID.String()+"|"+sp.start] {
			s.report["slots"].hit(false)
			continue
		}
		if sp.matchID == nil {
			// The referenced match doesn't exist (e.g. RR fixture numbering
			// shifted) — skip rather than guess; the plan is advisory.
			continue
		}
		start, _ := time.Parse(time.RFC3339, sp.start)
		end := start.Add(90 * time.Minute)
		mid := sp.matchID.String()
		if _, _, err := s.schedules.CreateSlot(ctx, s.organizerID, &s.orgID, schedule.CreateSlotRequest{
			TournamentID: tid.String(), CourtID: courtID.String(), MatchID: &mid,
			StartsAt: sp.start, EndsAt: end.UTC().Format(time.RFC3339),
		}); err != nil {
			// A schedule conflict on rerun is a skip, not a failure.
			if strings.Contains(err.Error(), "conflict") {
				s.report["slots"].hit(false)
				continue
			}
			return fmt.Errorf("renon seed: slot %s %s: %w", sp.court, sp.start, err)
		}
		s.report["slots"].hit(true)
	}
	return nil
}

func (s *renonSeeder) ensurePublished(ctx context.Context, tid uuid.UUID) error {
	var status string
	if err := s.pool.QueryRow(ctx, `SELECT status FROM tournaments WHERE id = $1`, tid).Scan(&status); err != nil {
		return err
	}
	if status == "published" {
		return nil
	}
	if _, err := s.tournaments.Publish(ctx, s.organizerID, tid, &s.orgID); err != nil {
		return fmt.Errorf("renon seed: publish: %w", err)
	}
	return nil
}
