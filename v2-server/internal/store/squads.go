package store

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// Squad mutation errors. The HTTP layer maps each one to a stable error code.
var (
	// ErrForbidden means the caller is a workspace member but neither an administrator nor a
	// maintainer of this squad.
	ErrForbidden = errors.New("forbidden")
	// ErrArchived means the squad is archived: its history stays readable but it accepts no edits.
	ErrArchived = errors.New("squad archived")
	// ErrMemberExists means the typed identity already has a live membership in this squad.
	ErrMemberExists = errors.New("member already active")
	// ErrInvalidMember means the requested member shape is not a typed human/agent reference.
	ErrInvalidMember = errors.New("invalid member")
	// ErrAgentUnavailable means the referenced Space Agent is missing, retired or in another tenant.
	ErrAgentUnavailable = errors.New("space agent unavailable")
	// ErrUserUnavailable means the referenced user is not an active member of this tenant.
	ErrUserUnavailable = errors.New("user unavailable")
)

// Member types. A squad member is a typed reference, never a bare member id.
const (
	MemberHuman = "human"
	MemberAgent = "agent"
)

// SquadSummary is a squad with live member counts. Archived squads are not listed.
type SquadSummary struct {
	ID              string     `json:"id"`
	Name            string     `json:"name"`
	Description     string     `json:"description"`
	Charter         string     `json:"charter"`
	LeaderAgentID   *string    `json:"leaderAgentId"`
	ArchivedAt      *time.Time `json:"archivedAt"`
	Version         int64      `json:"version"`
	CreatedBy       string     `json:"createdBy"`
	CreatedAt       time.Time  `json:"createdAt"`
	UpdatedAt       time.Time  `json:"updatedAt"`
	HumanCount      int64      `json:"humanCount"`
	AgentCount      int64      `json:"agentCount"`
	MaintainerCount int64      `json:"maintainerCount"`
}

// SquadMember is one typed membership. Ended memberships stay in the detail as history.
type SquadMember struct {
	ID           string     `json:"id"`
	Type         string     `json:"type"`
	UserID       *string    `json:"userId"`
	SpaceAgentID *string    `json:"spaceAgentId"`
	DisplayName  string     `json:"displayName"`
	Role         string     `json:"role"`
	Maintainer   bool       `json:"maintainer"`
	JoinedAt     time.Time  `json:"joinedAt"`
	EndedAt      *time.Time `json:"endedAt"`
}

// SquadDetail is a squad with its full membership history.
type SquadDetail struct {
	SquadSummary
	Members []SquadMember `json:"members"`
}

// SquadInput replaces a squad's editable fields. A nil LeaderAgentID clears the leader.
type SquadInput struct {
	Name          string
	Description   string
	Charter       string
	LeaderAgentID *string
	Version       int64
}

// SquadMemberInput adds a typed member. Exactly one identity field is set for the type.
type SquadMemberInput struct {
	Type         string
	UserID       string
	SpaceAgentID string
	Role         string
	Maintainer   bool
	Version      int64
}

// SquadMemberUpdate changes a live membership's role text and maintainer flag.
type SquadMemberUpdate struct {
	Role       string
	Maintainer bool
	Version    int64
}

// dbTX is the query surface shared by the pool and a transaction.
type dbTX interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}

const squadColumns = `s.id, s.name, s.description, s.charter, s.leader_agent_id::text, s.archived_at,
	s.version, s.created_by, s.created_at, s.updated_at,
	(SELECT count(*) FROM v2_squad_members m WHERE m.squad_id = s.id AND m.ended_at IS NULL AND m.member_type = 'human'),
	(SELECT count(*) FROM v2_squad_members m WHERE m.squad_id = s.id AND m.ended_at IS NULL AND m.member_type = 'agent'),
	(SELECT count(*) FROM v2_squad_members m WHERE m.squad_id = s.id AND m.ended_at IS NULL AND m.maintainer)`

func scanSquad(row pgx.Row) (SquadSummary, error) {
	var s SquadSummary
	e := row.Scan(&s.ID, &s.Name, &s.Description, &s.Charter, &s.LeaderAgentID, &s.ArchivedAt,
		&s.Version, &s.CreatedBy, &s.CreatedAt, &s.UpdatedAt,
		&s.HumanCount, &s.AgentCount, &s.MaintainerCount)
	if errors.Is(e, pgx.ErrNoRows) {
		return s, ErrNotFound
	}
	return s, e
}

// TenantMemberRole returns the caller's role when they are an active member of an active tenant.
func (s *Store) TenantMemberRole(ctx context.Context, tenant, user string) (string, error) {
	return tenantRole(ctx, s.pool, tenant, user)
}

func tenantRole(ctx context.Context, q dbTX, tenant, user string) (string, error) {
	var role string
	e := q.QueryRow(ctx, `
		SELECT m.role FROM memberships m
		JOIN users u ON u.id = m.user_id
		JOIN tenants t ON t.id = m.tenant_id
		WHERE m.tenant_id = $1 AND m.user_id = $2
			AND u.status = 'active' AND u.deleted_at IS NULL AND t.status = 'active'`, tenant, user).Scan(&role)
	if errors.Is(e, pgx.ErrNoRows) {
		return "", ErrNotFound
	}
	if e != nil {
		return "", fmt.Errorf("look up tenant role: %w", e)
	}
	return role, nil
}

// isAdministrator follows Cloud's role vocabulary; v2's development login uses "owner".
func isAdministrator(role string) bool { return role == "admin" || role == "owner" }

// requireManager checks current tenant membership and squad-level authority. Callers lock the
// squad row first so archived state and a concurrent maintainer change cannot race this check.
func requireManager(ctx context.Context, q dbTX, tenant, user, squadID string) error {
	role, e := tenantRole(ctx, q, tenant, user)
	if e != nil {
		return e
	}
	if isAdministrator(role) {
		return nil
	}
	var maintainer bool
	e = q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM v2_squad_members
			WHERE squad_id = $1 AND user_id = $2 AND member_type = 'human'
				AND maintainer AND ended_at IS NULL)`, squadID, user).Scan(&maintainer)
	if e != nil {
		return fmt.Errorf("look up maintainer: %w", e)
	}
	if !maintainer {
		return ErrForbidden
	}
	return nil
}

// lockSquad opens the mutation transaction: it locks the squad for the caller, rejects stale
// versions before any write and requires manager authority, so revocation, archiving and edits
// serialize on one row. Callers that let a member act on their own record (self-exit) use
// lockSquadBase and check authority themselves.
func lockSquad(ctx context.Context, tx pgx.Tx, tenant, user, id string, version int64) (SquadSummary, error) {
	s, e := lockSquadBase(ctx, tx, tenant, id, version)
	if e != nil {
		return s, e
	}
	if e := requireManager(ctx, tx, tenant, user, id); e != nil {
		return s, e
	}
	return s, nil
}

// lockSquadBase locks the squad row and enforces the archived and version gates, leaving the
// manager check to the caller.
func lockSquadBase(ctx context.Context, tx pgx.Tx, tenant, id string, version int64) (SquadSummary, error) {
	s, e := scanSquad(tx.QueryRow(ctx, `SELECT `+squadColumns+` FROM v2_squads s WHERE s.tenant_id = $1 AND s.id = $2 FOR UPDATE`, tenant, id))
	if e != nil {
		return s, e
	}
	if s.ArchivedAt != nil {
		return s, ErrArchived
	}
	if s.Version != version {
		return s, ErrConflict
	}
	return s, nil
}

func bumpSquad(ctx context.Context, tx pgx.Tx, id string) error {
	_, e := tx.Exec(ctx, `UPDATE v2_squads SET version = version + 1, updated_at = now() WHERE id = $1`, id)
	return e
}

func squadMembers(ctx context.Context, q dbTX, squadID string) ([]SquadMember, error) {
	rows, e := q.Query(ctx, `
		SELECT m.id, m.member_type, m.user_id::text, m.space_agent_id::text,
			COALESCE(u.display_name, a.name, ''), m.role, m.maintainer, m.joined_at, m.ended_at
		FROM v2_squad_members m
		LEFT JOIN users u ON u.id = m.user_id
		LEFT JOIN v2_space_agents a ON a.id = m.space_agent_id
		WHERE m.squad_id = $1
		ORDER BY m.joined_at, m.id`, squadID)
	if e != nil {
		return nil, fmt.Errorf("list squad members: %w", e)
	}
	defer rows.Close()
	members := []SquadMember{}
	for rows.Next() {
		var m SquadMember
		if e := rows.Scan(&m.ID, &m.Type, &m.UserID, &m.SpaceAgentID, &m.DisplayName, &m.Role,
			&m.Maintainer, &m.JoinedAt, &m.EndedAt); e != nil {
			return nil, fmt.Errorf("scan squad member: %w", e)
		}
		members = append(members, m)
	}
	return members, rows.Err()
}

func squadDetail(ctx context.Context, q dbTX, tenant, id string) (SquadDetail, error) {
	s, e := scanSquad(q.QueryRow(ctx, `SELECT `+squadColumns+` FROM v2_squads s WHERE s.tenant_id = $1 AND s.id = $2`, tenant, id))
	if e != nil {
		return SquadDetail{}, e
	}
	members, e := squadMembers(ctx, q, id)
	if e != nil {
		return SquadDetail{}, e
	}
	return SquadDetail{SquadSummary: s, Members: members}, nil
}

// Squads lists live squads in the tenant with live member counts.
func (s *Store) Squads(ctx context.Context, tenant string) ([]SquadSummary, error) {
	rows, e := s.pool.Query(ctx, `SELECT `+squadColumns+` FROM v2_squads s
		WHERE s.tenant_id = $1 AND s.archived_at IS NULL
		ORDER BY s.created_at, s.id`, tenant)
	if e != nil {
		return nil, fmt.Errorf("list squads: %w", e)
	}
	defer rows.Close()
	out := []SquadSummary{}
	for rows.Next() {
		v, e := scanSquad(rows)
		if e != nil {
			return nil, e
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// Squad returns one squad, archived or not, with its membership history.
func (s *Store) Squad(ctx context.Context, tenant, id string) (SquadDetail, error) {
	return squadDetail(ctx, s.pool, tenant, id)
}

// CreateSquad creates a squad and makes the creator its first human maintainer atomically.
func (s *Store) CreateSquad(ctx context.Context, tenant, user string, in SquadInput) (SquadDetail, error) {
	var out SquadDetail
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		role, e := tenantRole(ctx, tx, tenant, user)
		if e != nil {
			return e
		}
		if !isAdministrator(role) && role != "member" {
			return ErrForbidden
		}
		var id string
		if e := tx.QueryRow(ctx, `
			INSERT INTO v2_squads (tenant_id, name, description, charter, leader_agent_id, created_by)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING id`,
			tenant, in.Name, in.Description, in.Charter, in.LeaderAgentID, user).Scan(&id); e != nil {
			return fmt.Errorf("insert squad: %w", e)
		}
		if in.LeaderAgentID != nil {
			if e := ensureLeaderMember(ctx, tx, tenant, id, *in.LeaderAgentID); e != nil {
				return e
			}
		}
		if _, e := tx.Exec(ctx, `
			INSERT INTO v2_squad_members (squad_id, member_type, user_id, maintainer)
			VALUES ($1, 'human', $2, true)`, id, user); e != nil {
			return fmt.Errorf("insert creator membership: %w", e)
		}
		detail, e := squadDetail(ctx, tx, tenant, id)
		out = detail
		return e
	})
	return out, e
}

// UpdateSquad replaces editable fields under a version check. Setting a leader who is not yet a
// member adds the agent membership in the same transaction, as the ADR requires.
func (s *Store) UpdateSquad(ctx context.Context, tenant, user, id string, in SquadInput) (SquadDetail, error) {
	var out SquadDetail
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, e := lockSquad(ctx, tx, tenant, user, id, in.Version); e != nil {
			return e
		}
		if in.LeaderAgentID != nil {
			if e := ensureLeaderMember(ctx, tx, tenant, id, *in.LeaderAgentID); e != nil {
				return e
			}
		}
		if _, e := tx.Exec(ctx, `
			UPDATE v2_squads SET name = $2, description = $3, charter = $4, leader_agent_id = $5,
				version = version + 1, updated_at = now()
			WHERE id = $1`, id, in.Name, in.Description, in.Charter, in.LeaderAgentID); e != nil {
			return fmt.Errorf("update squad: %w", e)
		}
		detail, e := squadDetail(ctx, tx, tenant, id)
		out = detail
		return e
	})
	return out, e
}

// ensureLeaderMember adds an active agent membership when the leader is not already in the squad.
func ensureLeaderMember(ctx context.Context, tx pgx.Tx, tenant, squadID, agentID string) error {
	if e := requireActiveAgent(ctx, tx, tenant, agentID); e != nil {
		return e
	}
	var active bool
	if e := tx.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM v2_squad_members
			WHERE squad_id = $1 AND member_type = 'agent' AND space_agent_id = $2 AND ended_at IS NULL)`,
		squadID, agentID).Scan(&active); e != nil {
		return fmt.Errorf("look up leader membership: %w", e)
	}
	if active {
		return nil
	}
	if _, e := tx.Exec(ctx, `
		INSERT INTO v2_squad_members (squad_id, member_type, space_agent_id) VALUES ($1, 'agent', $2)`,
		squadID, agentID); e != nil {
		return fmt.Errorf("insert leader membership: %w", e)
	}
	return nil
}

func requireActiveAgent(ctx context.Context, q dbTX, tenant, id string) error {
	var active bool
	e := q.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM v2_space_agents
			WHERE id = $1 AND tenant_id = $2 AND kind = 'agent'
				AND status = 'active' AND observed_state = 'installed'
				AND requested_by_user_id IS NULL)`,
		id, tenant).Scan(&active)
	if e != nil {
		return fmt.Errorf("look up space agent: %w", e)
	}
	if !active {
		return ErrAgentUnavailable
	}
	return nil
}

func requireActiveUser(ctx context.Context, q dbTX, tenant, id string) error {
	var active bool
	e := q.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM memberships m
			JOIN users u ON u.id = m.user_id
			JOIN tenants t ON t.id = m.tenant_id
			WHERE m.tenant_id = $1 AND m.user_id = $2
				AND u.status = 'active' AND u.deleted_at IS NULL AND t.status = 'active')`,
		tenant, id).Scan(&active)
	if e != nil {
		return fmt.Errorf("look up user membership: %w", e)
	}
	if !active {
		return ErrUserUnavailable
	}
	return nil
}

// ArchiveSquad makes the squad read-only and ends every live membership, keeping the rows and the
// leader column cleared as history. Archived squads cannot be restored in this phase.
func (s *Store) ArchiveSquad(ctx context.Context, tenant, user, id string, version int64) (SquadDetail, error) {
	var out SquadDetail
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, e := lockSquad(ctx, tx, tenant, user, id, version); e != nil {
			return e
		}
		if _, e := tx.Exec(ctx, `
			UPDATE v2_squads SET archived_at = now(), leader_agent_id = NULL,
				version = version + 1, updated_at = now()
			WHERE id = $1`, id); e != nil {
			return fmt.Errorf("archive squad: %w", e)
		}
		if _, e := tx.Exec(ctx, `
			UPDATE v2_squad_members SET ended_at = now() WHERE squad_id = $1 AND ended_at IS NULL`, id); e != nil {
			return fmt.Errorf("end squad memberships: %w", e)
		}
		detail, e := squadDetail(ctx, tx, tenant, id)
		out = detail
		return e
	})
	return out, e
}

// AddSquadMember adds a typed member after checking current tenant/space eligibility. Rejoining
// after an ended grant inserts a new row; the old grant stays as history.
func (s *Store) AddSquadMember(ctx context.Context, tenant, user, id string, in SquadMemberInput) (SquadDetail, error) {
	var out SquadDetail
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, e := lockSquad(ctx, tx, tenant, user, id, in.Version); e != nil {
			return e
		}
		var userID, agentID *string
		switch in.Type {
		case MemberHuman:
			if in.SpaceAgentID != "" || in.UserID == "" {
				return ErrInvalidMember
			}
			if e := requireActiveUser(ctx, tx, tenant, in.UserID); e != nil {
				return e
			}
			userID = &in.UserID
		case MemberAgent:
			if in.UserID != "" || in.SpaceAgentID == "" || in.Maintainer {
				return ErrInvalidMember
			}
			if e := requireActiveAgent(ctx, tx, tenant, in.SpaceAgentID); e != nil {
				return e
			}
			agentID = &in.SpaceAgentID
		default:
			return ErrInvalidMember
		}
		var exists bool
		if e := tx.QueryRow(ctx, `
			SELECT EXISTS (
				SELECT 1 FROM v2_squad_members
				WHERE squad_id = $1 AND member_type = $2
					AND COALESCE(user_id, space_agent_id) = COALESCE($3::uuid, $4::uuid)
					AND ended_at IS NULL)`,
			id, in.Type, userID, agentID).Scan(&exists); e != nil {
			return fmt.Errorf("look up live membership: %w", e)
		}
		if exists {
			return ErrMemberExists
		}
		if _, e := tx.Exec(ctx, `
			INSERT INTO v2_squad_members (squad_id, member_type, user_id, space_agent_id, role, maintainer)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			id, in.Type, userID, agentID, in.Role, in.Maintainer); e != nil {
			return fmt.Errorf("insert squad member: %w", e)
		}
		if e := bumpSquad(ctx, tx, id); e != nil {
			return fmt.Errorf("bump squad version: %w", e)
		}
		detail, e := squadDetail(ctx, tx, tenant, id)
		out = detail
		return e
	})
	return out, e
}

// UpdateSquadMember edits a live membership. Only humans can hold squad maintenance authority.
func (s *Store) UpdateSquadMember(ctx context.Context, tenant, user, id, memberID string, in SquadMemberUpdate) (SquadDetail, error) {
	var out SquadDetail
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		if _, e := lockSquad(ctx, tx, tenant, user, id, in.Version); e != nil {
			return e
		}
		var memberType string
		e := tx.QueryRow(ctx, `
			SELECT member_type FROM v2_squad_members
			WHERE id = $1 AND squad_id = $2 AND ended_at IS NULL FOR UPDATE`,
			memberID, id).Scan(&memberType)
		if errors.Is(e, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if e != nil {
			return fmt.Errorf("look up squad member: %w", e)
		}
		if in.Maintainer && memberType != MemberHuman {
			return ErrInvalidMember
		}
		if _, e := tx.Exec(ctx, `
			UPDATE v2_squad_members SET role = $2, maintainer = $3 WHERE id = $1`,
			memberID, in.Role, in.Maintainer); e != nil {
			return fmt.Errorf("update squad member: %w", e)
		}
		if e := bumpSquad(ctx, tx, id); e != nil {
			return fmt.Errorf("bump squad version: %w", e)
		}
		detail, e := squadDetail(ctx, tx, tenant, id)
		out = detail
		return e
	})
	return out, e
}

// RemoveSquadMember ends a live membership. Removing the current leader clears the leader in the
// same transaction so no squad keeps a leader who is no longer a member. A human removing their
// own record is a self-exit: the ADR lets every active member leave, so the manager check is
// skipped for them and the membership keeps its history like any other removal.
func (s *Store) RemoveSquadMember(ctx context.Context, tenant, user, id, memberID string, version int64) (SquadDetail, error) {
	var out SquadDetail
	e := pgx.BeginFunc(ctx, s.pool, func(tx pgx.Tx) error {
		leader, e := lockSquadBase(ctx, tx, tenant, id, version)
		if e != nil {
			return e
		}
		var memberType string
		var userID, agentID *string
		e = tx.QueryRow(ctx, `
			SELECT member_type, user_id::text, space_agent_id::text FROM v2_squad_members
			WHERE id = $1 AND squad_id = $2 AND ended_at IS NULL FOR UPDATE`,
			memberID, id).Scan(&memberType, &userID, &agentID)
		if errors.Is(e, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if e != nil {
			return fmt.Errorf("look up squad member: %w", e)
		}
		selfExit := memberType == MemberHuman && userID != nil && *userID == user
		if !selfExit {
			if e := requireManager(ctx, tx, tenant, user, id); e != nil {
				return e
			}
		}
		if _, e := tx.Exec(ctx, `UPDATE v2_squad_members SET ended_at = now() WHERE id = $1`, memberID); e != nil {
			return fmt.Errorf("end squad membership: %w", e)
		}
		if memberType == MemberAgent && leader.LeaderAgentID != nil && agentID != nil && *leader.LeaderAgentID == *agentID {
			if _, e := tx.Exec(ctx, `UPDATE v2_squads SET leader_agent_id = NULL WHERE id = $1`, id); e != nil {
				return fmt.Errorf("clear squad leader: %w", e)
			}
		}
		if e := bumpSquad(ctx, tx, id); e != nil {
			return fmt.Errorf("bump squad version: %w", e)
		}
		detail, e := squadDetail(ctx, tx, tenant, id)
		out = detail
		return e
	})
	return out, e
}

// ValidSquadName reports whether a trimmed name is usable; the HTTP layer calls it before writes.
func ValidSquadName(name string) bool {
	trimmed := strings.TrimSpace(name)
	return trimmed != "" && trimmed == name && len([]rune(name)) <= 200
}
