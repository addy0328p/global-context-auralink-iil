package memory

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Export struct {
	Scope            Scope  `json:"scope"`
	Version          int64  `json:"memory_version"`
	Hash             string `json:"content_sha256"`
	Path             string `json:"markdown_path"`
	ContextPath      string `json:"context_path"`
	Bytes            int    `json:"markdown_bytes"`
	TokenEstimate    int    `json:"token_estimate"`
	TokenMeasurement string `json:"token_measurement"`
	Historical       bool   `json:"historical"`
	Synthetic        bool   `json:"synthetic"`
	Reused           bool   `json:"reused"`
}

// ExportSnapshot holds the scope lock to prevent an older snapshot replacing a
// newer exported file. PostgreSQL remains authoritative if a file write fails.
func ExportSnapshot(ctx context.Context, pool *pgxpool.Pool, s Snapshot, root string) (Export, error) {
	var result Export
	if err := validateScope(s.Scope); err != nil {
		return result, err
	}
	if s.Hash != hash([]byte(s.MD)) {
		return result, errors.New("cannot export a memory hash mismatch")
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		return result, errors.New("could not lock memory export")
	}
	defer tx.Rollback(context.Background())
	var current int64
	var scopeID int64
	err = tx.QueryRow(ctx, `SELECT p.current_memory_version,s.id FROM publication_state p JOIN subject_scopes s ON s.id=p.scope_id JOIN users u ON u.id=s.user_id
WHERE u.tenant_id=$1 AND u.namespace=$2 AND u.glid=$3 AND s.role=$4 FOR UPDATE OF p`, s.Scope.Tenant, s.Scope.Namespace, s.Scope.GLID, s.Scope.Role).Scan(&current, &scopeID)
	if err != nil {
		return result, errors.New("could not read export version")
	}
	if current != s.Version {
		return result, errors.New("snapshot changed before export; regenerate and retry")
	}
	canonical, err := loadSnapshot(ctx, tx, s.Scope, scopeID, current)
	if err != nil {
		return result, err
	}
	if canonical.Hash != s.Hash || canonical.MD != s.MD {
		return result, errors.New("export differs from the canonical database snapshot")
	}
	reused := s.Reused
	s = canonical
	s.Reused = reused
	dir := filepath.Join(root, s.Scope.Namespace, s.Scope.GLID)
	versionDir := filepath.Join(dir, "versions", fmt.Sprintf("v%06d", s.Version))
	if err := os.MkdirAll(versionDir, 0700); err != nil {
		return result, errors.New("could not create private export folder")
	}
	versionPath := filepath.Join(versionDir, s.Scope.Role+".md")
	if old, err := os.ReadFile(versionPath); err == nil {
		if string(old) != s.MD {
			return result, errors.New("immutable export already exists with different content")
		}
	} else if !os.IsNotExist(err) {
		return result, errors.New("could not inspect immutable export")
	} else if err := replaceFile(versionPath, []byte(s.MD)); err != nil {
		return result, err
	}
	path := filepath.Join(dir, s.Scope.Role+".md")
	if err := replaceFile(path, []byte(s.MD)); err != nil {
		return result, err
	}
	contextPath := filepath.Join(dir, s.Scope.Role+".context.json")
	b, err := marshal(s.Context)
	if err != nil {
		return result, errors.New("could not encode export context")
	}
	if err := replaceFile(contextPath, append(b, '\n')); err != nil {
		return result, err
	}
	if err := tx.Commit(ctx); err != nil {
		return result, errors.New("could not finish memory export")
	}
	return Export{Scope: s.Scope, Version: s.Version, Hash: s.Hash, Path: path, ContextPath: contextPath, Bytes: len(s.MD), TokenEstimate: s.Context.TokenEstimate, TokenMeasurement: s.Context.TokenMeasurement, Historical: s.Context.Historical, Synthetic: s.Context.Synthetic, Reused: s.Reused}, nil
}

func replaceFile(path string, b []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".memory-*.tmp")
	if err != nil {
		return errors.New("could not prepare export file")
	}
	name := f.Name()
	defer os.Remove(name)
	if _, err := f.Write(b); err != nil {
		f.Close()
		return errors.New("could not write export file")
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return errors.New("could not flush export file")
	}
	if err := f.Close(); err != nil {
		return errors.New("could not close export file")
	}
	if err := os.Rename(name, path); err != nil {
		return errors.New("could not replace export file")
	}
	return nil
}

func SaveManifest(root, namespace string, value any) error {
	if !namespacePattern.MatchString(namespace) {
		return errors.New("invalid export namespace")
	}
	dir := filepath.Join(root, namespace)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return errors.New("could not create manifest folder")
	}
	b, err := marshal(value)
	if err != nil {
		return errors.New("could not encode export manifest")
	}
	return replaceFile(filepath.Join(dir, "manifest.json"), append(b, '\n'))
}
