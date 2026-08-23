// Package event owns events (divisions), stages and groups within a tournament,
// and (via the draw module) draw generation for an event.
package event

import (
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/muslimalfatih/tourney-api/internal/draw"
	"github.com/muslimalfatih/tourney-api/internal/match"
	"github.com/muslimalfatih/tourney-api/internal/server"
	"github.com/muslimalfatih/tourney-api/internal/server/middleware"
)

type Handler struct {
	svc  *Service
	draw *draw.Service
}

func NewHandler(svc *Service, drawSvc *draw.Service) *Handler {
	return &Handler{svc: svc, draw: drawSvc}
}

func (h *Handler) RegisterPublic(rg *gin.RouterGroup) {
	rg.GET("/events/:id/bracket", h.getBracket)
	rg.GET("/events/:id/standings", h.getPublicStandings)
	rg.GET("/events/:id/groups", h.getGroupKnockout)
}

func (h *Handler) RegisterOrganizer(rg *gin.RouterGroup) {
	rg.GET("/tournaments/:id/events", h.list)
	rg.POST("/tournaments/:id/events", h.create)
	rg.GET("/events/:id", h.get)
	rg.PATCH("/events/:id", h.update)
	rg.DELETE("/events/:id", h.remove)
	// Draws are arranged manually per format: single_elim via the Match builder
	// (explicit round-1 pairs), round_robin by adding/removing fixtures, and
	// group_knockout by assigning teams to groups then building.
	rg.POST("/events/:id/bracket/build", h.buildBracket)
	rg.POST("/events/:id/matches", h.addManualMatch)
	rg.POST("/events/:id/matches/generate", h.generateFixtures)
	rg.DELETE("/matches/:id", h.deleteManualMatch)
	rg.PUT("/events/:id/groups", h.setGroups)
	rg.POST("/events/:id/resolve-groups", h.resolveGroups)
	rg.PATCH("/events/:id/groups/:groupId", h.setGroupAdvance)
	// Organizer read models: same data as the public reads but ungated (the
	// organizer builds/reviews the draw before publishing), org-scoped so an
	// organizer can only read their own event.
	rg.GET("/events/:id/bracket", h.getOrgBracket)
	rg.GET("/events/:id/standings", h.getOrgStandings)
	rg.GET("/events/:id/groups", h.getOrgGroups)
}

// buildBracketRequest is the Match-builder payload: the explicit round-1
// pairings (team ids, either side optional for a bye). Draws are always manual.
// Court/time aren't accepted here — scheduling is per-match from the Bracket tab.
type buildBracketRequest struct {
	Matches []draw.Pair `json:"matches"`
}

func orgScope(c *gin.Context) *uuid.UUID {
	if middleware.Role(c) == middleware.RoleSuperAdmin {
		return nil
	}
	id := middleware.OrgID(c)
	return &id
}

// --- Organizer ---

func (h *Handler) list(c *gin.Context) {
	tid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid tournament id"))
		return
	}
	items, err := h.svc.ListForTournament(c.Request.Context(), tid, orgScope(c))
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("tournament not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	if items == nil {
		items = []Event{}
	}
	server.OK(c, items)
}

func (h *Handler) create(c *gin.Context) {
	tid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid tournament id"))
		return
	}
	var req CreateEventRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid event payload"))
		return
	}
	ev, err := h.svc.Create(c.Request.Context(), tid, orgScope(c), req)
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("tournament not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.Created(c, ev)
}

func (h *Handler) get(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	ev, err := h.svc.Get(c.Request.Context(), id)
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("event not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, ev)
}

// update patches an event's public-facing config (category, gender, visibility,
// display name, strip order). Present pointers → apply; absent → leave. The two
// nullable text fields carry an explicit Set flag so "clear to null" is distinct
// from "unchanged".
func (h *Handler) update(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	var req UpdateEventRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid event payload"))
		return
	}
	// Pointers pass straight through: nil (absent/null) = leave unchanged;
	// present "" clears category/display_name to NULL (see repo Update).
	in := UpdateInput{
		Category:          req.Category,
		Gender:            req.Gender,
		IsPublic:          req.IsPublic,
		PublicDisplayName: req.PublicDisplayName,
		PublicOrder:       req.PublicOrder,
	}
	// json.RawMessage distinguishes absent (nil → unchanged) from present; a
	// present document is validated in the service before it reaches SQL.
	if req.Scoring != nil {
		raw := string(req.Scoring)
		in.Scoring = &raw
	}
	ev, err := h.svc.UpdatePublicSettings(c.Request.Context(), id, orgScope(c), in)
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("event not found"))
		return
	}
	var iv *match.InvalidScoreError
	if errors.As(err, &iv) {
		server.Error(c, (&server.AppError{
			Status: http.StatusUnprocessableEntity, Code: "invalid_scoring_config",
			Message: "the scoring configuration is invalid",
		}).WithDetails(map[string]any{"violations": iv.Violations}))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, ev)
}

func (h *Handler) remove(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	err = h.svc.Delete(c.Request.Context(), id, orgScope(c))
	if errors.Is(err, ErrNotFound) {
		server.Error(c, server.ErrNotFound("event not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.NoContent(c)
}

// buildBracket runs the Match builder: builds a single-elim bracket from the
// organizer's explicit round-1 pairings (manual only),
// overwriting any existing draw.
func (h *Handler) buildBracket(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	var req buildBracketRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid build payload"))
		return
	}
	count, err := h.draw.Build(c.Request.Context(), id, orgScope(c), draw.BuildInput{
		Pairs: req.Matches,
	})
	switch {
	case errors.Is(err, draw.ErrNotFound):
		server.Error(c, server.ErrNotFound("event not found"))
		return
	case errors.Is(err, draw.ErrForbidden):
		server.Error(c, server.ErrForbidden(""))
		return
	case errors.Is(err, draw.ErrNotEnough):
		server.Error(c, server.ErrValidation("need at least 2 participants"))
		return
	case errors.Is(err, draw.ErrInvalidPairs):
		server.Error(c, server.ErrValidation("pairings are invalid: a team is used twice, a match is half-filled, or an unknown team was supplied"))
		return
	case errors.Is(err, draw.ErrUnsupportedForm):
		server.Error(c, server.ErrValidation("the match builder supports single elimination only"))
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gin.H{"event_id": id, "matches": count, "generated": true})
}

// addManualMatch inserts one organizer-created fixture (round_robin manual setup).
func (h *Handler) addManualMatch(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	var req struct {
		TeamAID string `json:"team_a_id" binding:"required"`
		TeamBID string `json:"team_b_id" binding:"required"`
		// AllowRematch acknowledges a duplicate_fixture 409 for a DECIDED prior
		// fixture and creates the rematch anyway (audited). It never overrides
		// an unplayed duplicate.
		AllowRematch bool `json:"allow_rematch"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("both team ids are required"))
		return
	}
	a, err1 := uuid.Parse(req.TeamAID)
	b, err2 := uuid.Parse(req.TeamBID)
	if err1 != nil || err2 != nil {
		server.Error(c, server.ErrValidation("invalid team id"))
		return
	}
	mid, no, err := h.draw.AddManualMatch(c.Request.Context(), id, orgScope(c),
		middleware.UserID(c), a, b, req.AllowRematch)
	if handled := manualErr(c, err); handled {
		return
	}
	server.Created(c, gin.H{"id": mid, "match_no": no})
}

// generateFixtures fills in every missing round-robin pairing (idempotent —
// existing fixtures for a pair are kept, only missing ones are created).
func (h *Handler) generateFixtures(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	created, existing, err := h.draw.GenerateRoundRobinFixtures(c.Request.Context(), id, orgScope(c))
	if handled := manualErr(c, err); handled {
		return
	}
	server.OK(c, gin.H{"created": created, "existing": existing})
}

// deleteManualMatch removes one pending organizer-created fixture (round_robin).
func (h *Handler) deleteManualMatch(c *gin.Context) {
	mid, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid match id"))
		return
	}
	err = h.draw.DeleteManualMatch(c.Request.Context(), mid, orgScope(c))
	if handled := manualErr(c, err); handled {
		return
	}
	server.NoContent(c)
}

// setGroups assigns teams to groups and builds the group_knockout fixtures.
func (h *Handler) setGroups(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	var req struct {
		Groups []struct {
			Name         string   `json:"name"`
			AdvanceCount int      `json:"advance_count"`
			TeamIDs      []string `json:"team_ids"`
		} `json:"groups"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("invalid groups payload"))
		return
	}
	specs := make([]draw.GroupSpec, 0, len(req.Groups))
	for _, g := range req.Groups {
		ids := make([]uuid.UUID, 0, len(g.TeamIDs))
		for _, s := range g.TeamIDs {
			tid, perr := uuid.Parse(s)
			if perr != nil {
				server.Error(c, server.ErrValidation("invalid team id in group"))
				return
			}
			ids = append(ids, tid)
		}
		specs = append(specs, draw.GroupSpec{Name: g.Name, AdvanceCount: g.AdvanceCount, TeamIDs: ids})
	}
	count, err := h.draw.SetGroups(c.Request.Context(), id, orgScope(c), specs)
	if handled := manualErr(c, err); handled {
		return
	}
	server.OK(c, gin.H{"event_id": id, "matches": count, "generated": true})
}

// manualErr maps the manual-setup service errors to responses; true if handled.
func manualErr(c *gin.Context, err error) bool {
	var dup *draw.DuplicateFixtureError
	switch {
	case err == nil:
		return false
	case errors.As(err, &dup):
		// Stable duplicate contract: the existing fixture rides in the details
		// so the client can show it and (when rematchable) offer the explicit
		// allow_rematch confirmation.
		msg := "these teams already have an unplayed fixture"
		if dup.Rematchable {
			msg = "these teams already played; confirm to create a rematch"
		}
		server.Error(c, (&server.AppError{
			Status: http.StatusConflict, Code: "duplicate_fixture", Message: msg,
		}).WithDetails(gin.H{
			"match_id": dup.MatchID, "match_no": dup.MatchNo,
			"status": dup.Status, "rematchable": dup.Rematchable,
		}))
	case errors.Is(err, draw.ErrNotFound):
		server.Error(c, server.ErrNotFound("not found"))
	case errors.Is(err, draw.ErrForbidden):
		server.Error(c, server.ErrForbidden(""))
	case errors.Is(err, draw.ErrUnsupportedForm):
		server.Error(c, server.ErrValidation("not supported for this event's format"))
	case errors.Is(err, draw.ErrBadRequest):
		server.Error(c, server.ErrValidation("invalid request: check team assignments, group sizes, and advance counts"))
	default:
		server.Error(c, server.ErrInternal(""))
	}
	return true
}

// --- Public ---

func (h *Handler) getBracket(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	// Public route: only visible for a published tournament's public event.
	if err := h.draw.RequirePublic(c.Request.Context(), id); err != nil {
		publicReadErr(c, err)
		return
	}
	b, err := h.draw.GetBracket(c.Request.Context(), id)
	if errors.Is(err, draw.ErrNotFound) {
		server.Error(c, server.ErrNotFound("event not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, b)
}

// publicReadErr maps a public read-model gate/read error to a response.
func publicReadErr(c *gin.Context, err error) {
	if errors.Is(err, draw.ErrNotFound) {
		server.Error(c, server.ErrNotFound("event not found"))
		return
	}
	server.Error(c, server.ErrInternal(""))
}

func (h *Handler) getPublicStandings(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	if err := h.draw.RequirePublic(c.Request.Context(), id); err != nil {
		publicReadErr(c, err)
		return
	}
	standings, err := h.draw.GetStandings(c.Request.Context(), id)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gin.H{"event_id": id, "standings": standings})
}

// resolveGroups fills the knockout placeholders with the group finishers.
func (h *Handler) resolveGroups(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	filled, err := h.draw.ResolveGroups(c.Request.Context(), id, orgScope(c))
	switch {
	case errors.Is(err, draw.ErrNotFound):
		server.Error(c, server.ErrNotFound("event not found"))
		return
	case errors.Is(err, draw.ErrForbidden):
		server.Error(c, server.ErrForbidden(""))
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gin.H{"event_id": id, "filled": filled})
}

// setGroupAdvanceRequest is the payload for editing how many teams advance.
type setGroupAdvanceRequest struct {
	AdvanceCount int `json:"advance_count" binding:"required,min=1"`
}

// setGroupAdvance edits one group's advance_count (top-N advance to knockout).
func (h *Handler) setGroupAdvance(c *gin.Context) {
	groupID, err := uuid.Parse(c.Param("groupId"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid group id"))
		return
	}
	var req setGroupAdvanceRequest
	if err := c.ShouldBindJSON(&req); err != nil {
		server.Error(c, server.ErrValidation("advance_count must be a whole number ≥ 1"))
		return
	}
	eventID, advance, err := h.draw.SetGroupAdvance(c.Request.Context(), groupID, orgScope(c), req.AdvanceCount)
	switch {
	case errors.Is(err, draw.ErrNotFound):
		server.Error(c, server.ErrNotFound("group not found"))
		return
	case errors.Is(err, draw.ErrForbidden):
		server.Error(c, server.ErrForbidden(""))
		return
	case errors.Is(err, draw.ErrInvalidPairs):
		server.Error(c, server.ErrValidation("advance_count must be at least 1"))
		return
	case err != nil:
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gin.H{"event_id": eventID, "group_id": groupID, "advance_count": advance})
}

func (h *Handler) getGroupKnockout(c *gin.Context) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return
	}
	if err := h.draw.RequirePublic(c.Request.Context(), id); err != nil {
		publicReadErr(c, err)
		return
	}
	gk, err := h.draw.GetGroupKnockout(c.Request.Context(), id)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gk)
}

// --- Organizer read models (ungated, org-scoped) ---
// The organizer reviews the bracket/standings/groups while the tournament is
// still a draft, so these skip the public is_public/published gate but verify
// the caller's org owns the event.

func (h *Handler) getOrgBracket(c *gin.Context) {
	id, ok := h.authorizeRead(c)
	if !ok {
		return
	}
	b, err := h.draw.GetBracket(c.Request.Context(), id)
	if errors.Is(err, draw.ErrNotFound) {
		server.Error(c, server.ErrNotFound("event not found"))
		return
	}
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, b)
}

func (h *Handler) getOrgStandings(c *gin.Context) {
	id, ok := h.authorizeRead(c)
	if !ok {
		return
	}
	standings, err := h.draw.GetStandings(c.Request.Context(), id)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gin.H{"event_id": id, "standings": standings})
}

func (h *Handler) getOrgGroups(c *gin.Context) {
	id, ok := h.authorizeRead(c)
	if !ok {
		return
	}
	gk, err := h.draw.GetGroupKnockout(c.Request.Context(), id)
	if err != nil {
		server.Error(c, server.ErrInternal(""))
		return
	}
	server.OK(c, gk)
}

// authorizeRead parses the event id and verifies org ownership. On failure it
// writes the response and returns ok=false.
func (h *Handler) authorizeRead(c *gin.Context) (uuid.UUID, bool) {
	id, err := uuid.Parse(c.Param("id"))
	if err != nil {
		server.Error(c, server.ErrBadRequest("invalid event id"))
		return uuid.Nil, false
	}
	if err := h.svc.AuthorizeEvent(c.Request.Context(), id, orgScope(c)); err != nil {
		if errors.Is(err, ErrNotFound) {
			server.Error(c, server.ErrNotFound("event not found"))
		} else {
			server.Error(c, server.ErrInternal(""))
		}
		return uuid.Nil, false
	}
	return id, true
}
