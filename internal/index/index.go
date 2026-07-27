// Package index maintains the SQLite FTS5 catalog of markdown documents and
// their per-document read state.
package index

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/ahaley/markroom/internal/config"
)

// Directory names never descended into during a scan.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "bin": true, "obj": true,
	"dist": true, "build": true, "target": true, "__pycache__": true,
}

// Store owns the SQLite index of documents and read state.
type Store struct {
	db *sql.DB
}

// Open opens (creating if necessary) the index database at path.
func Open(path string) (*Store, error) {
	dsn := "file:" + filepath.ToSlash(path) +
		"?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// modernc/sqlite misbehaves under concurrent writers on one connection pool.
	db.SetMaxOpenConns(1)
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

func migrate(db *sql.DB) error {
	_, err := db.Exec(`
CREATE TABLE IF NOT EXISTS docs(
  id      INTEGER PRIMARY KEY,
  root    TEXT NOT NULL,
  relpath TEXT NOT NULL,
  title   TEXT NOT NULL,
  hash    TEXT NOT NULL,
  mtime   INTEGER NOT NULL,
  size    INTEGER NOT NULL,
  words   INTEGER NOT NULL,
  UNIQUE(root, relpath)
);
CREATE TABLE IF NOT EXISTS read_state(
  doc_id    INTEGER PRIMARY KEY,
  read_at   INTEGER NOT NULL,
  hash_read TEXT NOT NULL
);
CREATE VIRTUAL TABLE IF NOT EXISTS docs_fts USING fts5(title, body);
CREATE TABLE IF NOT EXISTS peer_state(
  peer          TEXT PRIMARY KEY,
  server_id     TEXT NOT NULL DEFAULT '',
  last_try_at   INTEGER NOT NULL DEFAULT 0,
  last_ok_at    INTEGER NOT NULL DEFAULT 0,
  last_error    TEXT NOT NULL DEFAULT '',
  manifest_etag TEXT NOT NULL DEFAULT ''
);`)
	return err
}

// PeerState is what we remember about the last conversation with a peer.
// It lives here rather than in config.json because it changes on every sync
// cycle, and a daemon rewriting the config that often would keep colliding
// with whatever the operator is doing at the command line.
type PeerState struct {
	Peer         string
	ServerID     string
	LastTry      time.Time
	LastOK       time.Time
	LastError    string
	ManifestETag string
}

// Synced reports whether the last attempt succeeded.
func (p PeerState) Synced() bool { return p.LastError == "" && !p.LastOK.IsZero() }

func (s *Store) PeerStates(ctx context.Context) (map[string]PeerState, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT peer, server_id, last_try_at, last_ok_at, last_error, manifest_etag FROM peer_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]PeerState{}
	for rows.Next() {
		var p PeerState
		var try, ok int64
		if err := rows.Scan(&p.Peer, &p.ServerID, &try, &ok, &p.LastError, &p.ManifestETag); err != nil {
			return nil, err
		}
		if try > 0 {
			p.LastTry = time.Unix(try, 0)
		}
		if ok > 0 {
			p.LastOK = time.Unix(ok, 0)
		}
		out[p.Peer] = p
	}
	return out, rows.Err()
}

func (s *Store) SetPeerState(ctx context.Context, p PeerState) error {
	var try, ok int64
	if !p.LastTry.IsZero() {
		try = p.LastTry.Unix()
	}
	if !p.LastOK.IsZero() {
		ok = p.LastOK.Unix()
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO peer_state(peer, server_id, last_try_at, last_ok_at, last_error, manifest_etag)
VALUES(?,?,?,?,?,?)
ON CONFLICT(peer) DO UPDATE SET
  server_id=excluded.server_id, last_try_at=excluded.last_try_at,
  last_ok_at=excluded.last_ok_at, last_error=excluded.last_error,
  manifest_etag=excluded.manifest_etag`,
		p.Peer, p.ServerID, try, ok, p.LastError, p.ManifestETag)
	return err
}

func (s *Store) DeletePeerState(ctx context.Context, peer string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM peer_state WHERE peer=?`, peer)
	return err
}

// Doc is one indexed markdown file joined with its read state.
type Doc struct {
	ID      int64
	Root    string
	RelPath string
	Title   string
	Hash    string
	MTime   time.Time
	Words   int
	Status  string // "new", "updated", "read"
	Snippet string // populated by search only (trusted HTML from FTS snippet)
}

// IsMarkdown reports whether the filename has a markdown extension.
func IsMarkdown(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".md" || ext == ".markdown"
}

// SkipDirName reports whether a walk should refuse to descend into a
// directory of this name. Exported so anything else walking a root — the
// peer manifest's asset listing, say — hides the same things the scanner does.
func SkipDirName(name string) bool {
	return strings.HasPrefix(name, ".") || skipDirs[strings.ToLower(name)]
}

// dbtx is the intersection of *sql.DB and *sql.Tx the write paths need, so
// the same statements run standalone or inside a scan transaction.
type dbtx interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// ScanRoot walks one root, upserts changed docs, and removes vanished ones,
// all in one transaction so a mid-scan crash can't leave docs and docs_fts
// disagreeing. It returns the number of documents now indexed under the root.
func (s *Store) ScanRoot(ctx context.Context, root config.Root) (int, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	count, err := scanRoot(ctx, tx, root)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return count, nil
}

func scanRoot(ctx context.Context, q dbtx, root config.Root) (int, error) {
	seen := map[string]bool{}
	err := filepath.WalkDir(root.Path, func(path string, d fs.DirEntry, err error) error {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}
		if err != nil {
			return nil // unreadable entry: skip, keep walking
		}
		if d.IsDir() {
			if path != root.Path && SkipDirName(d.Name()) {
				return filepath.SkipDir
			}
			return nil
		}
		if !IsMarkdown(d.Name()) {
			return nil
		}
		rel, err := filepath.Rel(root.Path, path)
		if err != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		seen[rel] = true

		info, err := d.Info()
		if err != nil {
			return nil
		}
		var mtime, size int64
		row := q.QueryRowContext(ctx, `SELECT mtime, size FROM docs WHERE root=? AND relpath=?`, root.Name, rel)
		if scanErr := row.Scan(&mtime, &size); scanErr == nil &&
			mtime == info.ModTime().Unix() && size == info.Size() {
			return nil // unchanged
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		return upsertDoc(ctx, q, root.Name, rel, content, info.ModTime(), info.Size())
	})
	if err != nil {
		return 0, err
	}

	// Purge rows for files that no longer exist.
	rows, err := q.QueryContext(ctx, `SELECT id, relpath FROM docs WHERE root=?`, root.Name)
	if err != nil {
		return 0, err
	}
	var dead []int64
	count := 0
	for rows.Next() {
		var id int64
		var rel string
		if err := rows.Scan(&id, &rel); err != nil {
			rows.Close()
			return 0, err
		}
		if seen[rel] {
			count++
		} else {
			dead = append(dead, id)
		}
	}
	rows.Close()
	for _, id := range dead {
		if err := deleteDoc(ctx, q, id); err != nil {
			return 0, err
		}
	}
	return count, nil
}

// ScanAll scans every root, logging (not returning) per-root failures so one
// unreadable root can't stop the others from staying fresh.
func (s *Store) ScanAll(ctx context.Context, roots []config.Root) {
	for _, r := range roots {
		if _, err := s.ScanRoot(ctx, r); err != nil {
			slog.Warn("scan failed", "root", r.Name, "err", err)
		}
	}
}

func (s *Store) UpsertDoc(ctx context.Context, rootName, rel string, content []byte, mtime time.Time, size int64) error {
	return upsertDoc(ctx, s.db, rootName, rel, content, mtime, size)
}

func upsertDoc(ctx context.Context, q dbtx, rootName, rel string, content []byte, mtime time.Time, size int64) error {
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	body := StripFrontmatter(string(content))
	title := extractTitle(body, rel)
	words := len(strings.Fields(body))

	var id int64
	err := q.QueryRowContext(ctx, `SELECT id FROM docs WHERE root=? AND relpath=?`, rootName, rel).Scan(&id)
	switch {
	case err == nil:
		if _, err := q.ExecContext(ctx, `UPDATE docs SET title=?, hash=?, mtime=?, size=?, words=? WHERE id=?`,
			title, hash, mtime.Unix(), size, words, id); err != nil {
			return err
		}
		if _, err := q.ExecContext(ctx, `DELETE FROM docs_fts WHERE rowid=?`, id); err != nil {
			return err
		}
	case errors.Is(err, sql.ErrNoRows):
		res, err := q.ExecContext(ctx, `INSERT INTO docs(root, relpath, title, hash, mtime, size, words) VALUES(?,?,?,?,?,?,?)`,
			rootName, rel, title, hash, mtime.Unix(), size, words)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
	default:
		return err
	}
	_, err = q.ExecContext(ctx, `INSERT INTO docs_fts(rowid, title, body) VALUES(?,?,?)`, id, title, body)
	return err
}

func deleteDoc(ctx context.Context, q dbtx, id int64) error {
	for _, stmt := range []string{
		`DELETE FROM docs_fts WHERE rowid=?`,
		`DELETE FROM read_state WHERE doc_id=?`,
		`DELETE FROM docs WHERE id=?`,
	} {
		if _, err := q.ExecContext(ctx, stmt, id); err != nil {
			return err
		}
	}
	return nil
}

// PurgeRoot removes every indexed document belonging to a root, atomically.
func (s *Store) PurgeRoot(ctx context.Context, rootName string) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT id FROM docs WHERE root=?`, rootName)
	if err != nil {
		return err
	}
	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return err
		}
		ids = append(ids, id)
	}
	rows.Close()
	for _, id := range ids {
		if err := deleteDoc(ctx, tx, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (s *Store) RootStats(ctx context.Context, rootName string) (total, unread int, err error) {
	err = s.db.QueryRowContext(ctx, `
SELECT COUNT(*),
       COALESCE(SUM(CASE WHEN r.hash_read IS NULL OR r.hash_read <> d.hash THEN 1 ELSE 0 END), 0)
FROM docs d LEFT JOIN read_state r ON r.doc_id = d.id
WHERE d.root=?`, rootName).Scan(&total, &unread)
	return
}

// RootCount holds per-root document totals.
type RootCount struct {
	Total  int
	Unread int
}

// AllRootStats returns counts for every root that has indexed docs.
func (s *Store) AllRootStats(ctx context.Context) (map[string]RootCount, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT d.root, COUNT(*),
       COALESCE(SUM(CASE WHEN r.hash_read IS NULL OR r.hash_read <> d.hash THEN 1 ELSE 0 END), 0)
FROM docs d LEFT JOIN read_state r ON r.doc_id = d.id
GROUP BY d.root`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := map[string]RootCount{}
	for rows.Next() {
		var name string
		var c RootCount
		if err := rows.Scan(&name, &c.Total, &c.Unread); err != nil {
			return nil, err
		}
		counts[name] = c
	}
	return counts, rows.Err()
}

// DocMeta is one document's catalog entry without its body — what a peer
// needs to decide whether it already has the file.
type DocMeta struct {
	RelPath string
	Title   string
	Hash    string
	MTime   int64
	Size    int64
	Words   int
}

// AllDocMeta returns every indexed document grouped by root, each group
// ordered by path. One query serves a whole manifest.
func (s *Store) AllDocMeta(ctx context.Context) (map[string][]DocMeta, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT root, relpath, title, hash, mtime, size, words FROM docs
ORDER BY root, relpath`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string][]DocMeta{}
	for rows.Next() {
		var root string
		var m DocMeta
		if err := rows.Scan(&root, &m.RelPath, &m.Title, &m.Hash, &m.MTime, &m.Size, &m.Words); err != nil {
			return nil, err
		}
		out[root] = append(out[root], m)
	}
	return out, rows.Err()
}

// DocHashes returns relpath -> content hash for one root, so a sync can tell
// at a glance which of a peer's documents it is already holding.
func (s *Store) DocHashes(ctx context.Context, rootName string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT relpath, hash FROM docs WHERE root=?`, rootName)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var rel, hash string
		if err := rows.Scan(&rel, &hash); err != nil {
			return nil, err
		}
		out[rel] = hash
	}
	return out, rows.Err()
}

const docStatusExpr = `CASE
  WHEN r.hash_read IS NULL THEN 'new'
  WHEN r.hash_read <> d.hash THEN 'updated'
  ELSE 'read' END`

// ListDocs returns docs (every root when rootName is empty), unread first,
// then most recently modified.
func (s *Store) ListDocs(ctx context.Context, rootName string) ([]Doc, error) {
	where, args := "", []any(nil)
	if rootName != "" {
		where, args = "WHERE d.root = ?", []any{rootName}
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT d.id, d.root, d.relpath, d.title, d.hash, d.mtime, d.words, `+docStatusExpr+`
FROM docs d LEFT JOIN read_state r ON r.doc_id = d.id
`+where+`
ORDER BY (r.hash_read IS NULL OR r.hash_read <> d.hash) DESC, d.mtime DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDocs(rows, false)
}

// Search runs an FTS5 match built from user input, optionally scoped to one root.
func (s *Store) Search(ctx context.Context, query, rootName string) ([]Doc, error) {
	var terms []string
	for _, t := range strings.Fields(query) {
		terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"*`)
	}
	if len(terms) == 0 {
		return nil, nil
	}
	where, args := "WHERE docs_fts MATCH ?", []any{strings.Join(terms, " ")}
	if rootName != "" {
		where += " AND d.root = ?"
		args = append(args, rootName)
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT d.id, d.root, d.relpath, d.title, d.hash, d.mtime, d.words, `+docStatusExpr+`,
       snippet(docs_fts, 1, '<mark>', '</mark>', ' … ', 18)
FROM docs_fts
JOIN docs d ON d.id = docs_fts.rowid
LEFT JOIN read_state r ON r.doc_id = d.id
`+where+`
ORDER BY rank`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDocs(rows, true)
}

func collectDocs(rows *sql.Rows, withSnippet bool) ([]Doc, error) {
	var docs []Doc
	for rows.Next() {
		var d Doc
		var mtime int64
		dest := []any{&d.ID, &d.Root, &d.RelPath, &d.Title, &d.Hash, &mtime, &d.Words, &d.Status}
		if withSnippet {
			dest = append(dest, &d.Snippet)
		}
		if err := rows.Scan(dest...); err != nil {
			return nil, err
		}
		d.MTime = time.Unix(mtime, 0)
		docs = append(docs, d)
	}
	return docs, rows.Err()
}

func (s *Store) GetDoc(ctx context.Context, rootName, rel string) (*Doc, error) {
	row := s.db.QueryRowContext(ctx, `
SELECT d.id, d.root, d.relpath, d.title, d.hash, d.mtime, d.words, `+docStatusExpr+`
FROM docs d LEFT JOIN read_state r ON r.doc_id = d.id
WHERE d.root=? AND d.relpath=?`, rootName, rel)
	var d Doc
	var mtime int64
	err := row.Scan(&d.ID, &d.Root, &d.RelPath, &d.Title, &d.Hash, &mtime, &d.Words, &d.Status)
	if err != nil {
		return nil, err
	}
	d.MTime = time.Unix(mtime, 0)
	return &d, nil
}

func (s *Store) MarkRead(ctx context.Context, docID int64, hash string) error {
	_, err := s.db.ExecContext(ctx, `
INSERT INTO read_state(doc_id, read_at, hash_read) VALUES(?,?,?)
ON CONFLICT(doc_id) DO UPDATE SET read_at=excluded.read_at, hash_read=excluded.hash_read`,
		docID, time.Now().Unix(), hash)
	return err
}

func (s *Store) MarkUnread(ctx context.Context, docID int64) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM read_state WHERE doc_id=?`, docID)
	return err
}

// StripFrontmatter removes a leading YAML frontmatter block if present.
func StripFrontmatter(s string) string {
	if !strings.HasPrefix(s, "---") {
		return s
	}
	rest := s[3:]
	if !strings.HasPrefix(rest, "\n") && !strings.HasPrefix(rest, "\r\n") {
		return s
	}
	if i := strings.Index(rest, "\n---"); i >= 0 {
		after := rest[i+4:]
		if j := strings.IndexByte(after, '\n'); j >= 0 {
			return after[j+1:]
		}
		return ""
	}
	return s
}

// extractTitle returns the first ATX h1 heading, or the filename.
func extractTitle(body, rel string) string {
	for i, line := range strings.Split(body, "\n") {
		if i > 100 {
			break
		}
		line = strings.TrimSpace(line)
		if t, ok := strings.CutPrefix(line, "# "); ok {
			t = strings.TrimSpace(strings.Trim(t, "#*_` "))
			if t != "" {
				return t
			}
		}
	}
	base := filepath.Base(rel)
	return strings.TrimSuffix(base, filepath.Ext(base))
}
