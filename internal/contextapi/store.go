package contextapi

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/rohankamewar2/global-context-auralink-iil/internal/memory"
)

var ErrNotFound = errors.New("session unavailable")

type Session struct {
	ID             string       `json:"session_id"`
	Scope          memory.Scope `json:"scope"`
	Principal      string       `json:"-"`
	Channel        string       `json:"channel"`
	Thread         string       `json:"active_thread_ref"`
	InitialVersion int64        `json:"initial_memory_version"`
	CapabilityHash string       `json:"-"`
	ResumedFrom    string       `json:"resumed_from_id,omitempty"`
	ExpiresAt      time.Time    `json:"expires_at"`
}

type Store interface {
	Published(context.Context, memory.Scope) (memory.Snapshot, int64, error)
	Create(context.Context, Session) (Session, string, error)
	Session(context.Context, string) (Session, error)
}

type PGStore struct{ Pool *pgxpool.Pool }

func (s PGStore) Published(ctx context.Context, scope memory.Scope) (memory.Snapshot, int64, error) {
	return memory.ReadPublished(ctx, s.Pool, scope)
}

func randomToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
func digest(v string) string { h := sha256.Sum256([]byte(v)); return hex.EncodeToString(h[:]) }

func (s PGStore) Create(ctx context.Context, v Session) (Session, string, error) {
	id, err := randomToken()
	if err != nil {
		return Session{}, "", errors.New("could not create session identity")
	}
	capability, err := randomToken()
	if err != nil {
		return Session{}, "", errors.New("could not create session capability")
	}
	v.ID = id
	v.CapabilityHash = digest(capability)
	// Scope and thread ownership are resolved in SQL and reinforced by composite FKs.
	err = s.Pool.QueryRow(ctx, `INSERT INTO context_sessions
 (id,principal_id,scope_id,channel,thread_id,initial_memory_version,capability_sha256,resumed_from_id,expires_at)
 SELECT $1,$2,s.id,$3,t.id,$4,$5,NULLIF($6,''),clock_timestamp()+interval '30 minutes'
 FROM users u JOIN subject_scopes s ON s.user_id=u.id
 LEFT JOIN conversation_threads t ON t.scope_id=s.id AND t.thread_ref=$7
 WHERE u.tenant_id=$8 AND u.namespace=$9 AND u.glid=$10 AND s.role=$11
 AND ($7='' OR t.id IS NOT NULL)
 RETURNING expires_at`, v.ID, v.Principal, v.Channel, v.InitialVersion, v.CapabilityHash, v.ResumedFrom, v.Thread, v.Scope.Tenant, v.Scope.Namespace, v.Scope.GLID, v.Scope.Role).Scan(&v.ExpiresAt)
	if err != nil {
		return Session{}, "", errors.New("could not persist bound context session")
	}
	return v, capability, nil
}

func (s PGStore) Session(ctx context.Context, id string) (Session, error) {
	var v Session
	err := s.Pool.QueryRow(ctx, `SELECT x.id,x.principal_id,u.tenant_id,u.namespace,u.glid,s.role,x.channel,
 COALESCE(t.thread_ref,''),x.initial_memory_version,x.capability_sha256,COALESCE(x.resumed_from_id,''),x.expires_at
 FROM context_sessions x JOIN subject_scopes s ON s.id=x.scope_id JOIN users u ON u.id=s.user_id
 LEFT JOIN conversation_threads t ON t.id=x.thread_id AND t.scope_id=x.scope_id WHERE x.id=$1 AND x.expires_at>clock_timestamp()`, id).Scan(&v.ID, &v.Principal, &v.Scope.Tenant, &v.Scope.Namespace, &v.Scope.GLID, &v.Scope.Role, &v.Channel, &v.Thread, &v.InitialVersion, &v.CapabilityHash, &v.ResumedFrom, &v.ExpiresAt)
	if err != nil {
		return Session{}, ErrNotFound
	}
	return v, nil
}
