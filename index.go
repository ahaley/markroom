package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

// Directory names never descended into during a scan.
var skipDirs = map[string]bool{
	"node_modules": true, "vendor": true, "bin": true, "obj": true,
	"dist": true, "build": true, "target": true, "__pycache__": true,
}

func openDB() (*sql.DB, error) {
	dir, err := configDir()
	if err != nil {
		return nil, err
	}
	dsn := "file:" + filepath.ToSlash(filepath.Join(dir, "index.db")) +
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
	return db, nil
}

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
CREATE VIRTUAL TABLE IF NOT EXISTS docs_fts USING fts5(title, body);`)
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

func isMarkdown(name string) bool {
	ext := strings.ToLower(filepath.Ext(name))
	return ext == ".md" || ext == ".markdown"
}

// scanRoot walks one root, upserts changed docs, and removes vanished ones.
// It returns the number of documents now indexed under the root.
func scanRoot(db *sql.DB, root Root) (int, error) {
	seen := map[string]bool{}
	err := filepath.WalkDir(root.Path, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable entry: skip, keep walking
		}
		if d.IsDir() {
			name := d.Name()
			if path != root.Path && (strings.HasPrefix(name, ".") || skipDirs[strings.ToLower(name)]) {
				return filepath.SkipDir
			}
			return nil
		}
		if !isMarkdown(d.Name()) {
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
		row := db.QueryRow(`SELECT mtime, size FROM docs WHERE root=? AND relpath=?`, root.Name, rel)
		if scanErr := row.Scan(&mtime, &size); scanErr == nil &&
			mtime == info.ModTime().Unix() && size == info.Size() {
			return nil // unchanged
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		return upsertDoc(db, root.Name, rel, content, info.ModTime(), info.Size())
	})
	if err != nil {
		return 0, err
	}

	// Purge rows for files that no longer exist.
	rows, err := db.Query(`SELECT id, relpath FROM docs WHERE root=?`, root.Name)
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
		if err := deleteDoc(db, id); err != nil {
			return 0, err
		}
	}
	return count, nil
}

func scanAll(db *sql.DB, cfg *Config) {
	for _, r := range cfg.Roots {
		if _, err := scanRoot(db, r); err != nil {
			fmt.Fprintf(os.Stderr, "markroom: scan %s: %v\n", r.Name, err)
		}
	}
}

func upsertDoc(db *sql.DB, rootName, rel string, content []byte, mtime time.Time, size int64) error {
	sum := sha256.Sum256(content)
	hash := hex.EncodeToString(sum[:])
	body := stripFrontmatter(string(content))
	title := extractTitle(body, rel)
	words := len(strings.Fields(body))

	var id int64
	err := db.QueryRow(`SELECT id FROM docs WHERE root=? AND relpath=?`, rootName, rel).Scan(&id)
	switch err {
	case nil:
		if _, err := db.Exec(`UPDATE docs SET title=?, hash=?, mtime=?, size=?, words=? WHERE id=?`,
			title, hash, mtime.Unix(), size, words, id); err != nil {
			return err
		}
		if _, err := db.Exec(`DELETE FROM docs_fts WHERE rowid=?`, id); err != nil {
			return err
		}
	case sql.ErrNoRows:
		res, err := db.Exec(`INSERT INTO docs(root, relpath, title, hash, mtime, size, words) VALUES(?,?,?,?,?,?,?)`,
			rootName, rel, title, hash, mtime.Unix(), size, words)
		if err != nil {
			return err
		}
		id, _ = res.LastInsertId()
	default:
		return err
	}
	_, err = db.Exec(`INSERT INTO docs_fts(rowid, title, body) VALUES(?,?,?)`, id, title, body)
	return err
}

func deleteDoc(db *sql.DB, id int64) error {
	for _, q := range []string{
		`DELETE FROM docs_fts WHERE rowid=?`,
		`DELETE FROM read_state WHERE doc_id=?`,
		`DELETE FROM docs WHERE id=?`,
	} {
		if _, err := db.Exec(q, id); err != nil {
			return err
		}
	}
	return nil
}

func purgeRoot(db *sql.DB, rootName string) error {
	rows, err := db.Query(`SELECT id FROM docs WHERE root=?`, rootName)
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
		if err := deleteDoc(db, id); err != nil {
			return err
		}
	}
	return nil
}

func rootStats(db *sql.DB, rootName string) (total, unread int, err error) {
	err = db.QueryRow(`
SELECT COUNT(*),
       SUM(CASE WHEN r.hash_read IS NULL OR r.hash_read <> d.hash THEN 1 ELSE 0 END)
FROM docs d LEFT JOIN read_state r ON r.doc_id = d.id
WHERE d.root=?`, rootName).Scan(&total, &nullableInt{&unread})
	return
}

// nullableInt scans SQL NULL (SUM over zero rows) as 0.
type nullableInt struct{ p *int }

func (n *nullableInt) Scan(v any) error {
	if v == nil {
		*n.p = 0
		return nil
	}
	switch t := v.(type) {
	case int64:
		*n.p = int(t)
	case float64:
		*n.p = int(t)
	default:
		return fmt.Errorf("unexpected type %T", v)
	}
	return nil
}

const docStatusExpr = `CASE
  WHEN r.hash_read IS NULL THEN 'new'
  WHEN r.hash_read <> d.hash THEN 'updated'
  ELSE 'read' END`

// listDocs returns every doc, unread first, then most recently modified.
func listDocs(db *sql.DB) ([]Doc, error) {
	rows, err := db.Query(`
SELECT d.id, d.root, d.relpath, d.title, d.hash, d.mtime, d.words, ` + docStatusExpr + `
FROM docs d LEFT JOIN read_state r ON r.doc_id = d.id
ORDER BY (r.hash_read IS NULL OR r.hash_read <> d.hash) DESC, d.mtime DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return collectDocs(rows, false)
}

// searchDocs runs an FTS5 match built from user input.
func searchDocs(db *sql.DB, query string) ([]Doc, error) {
	var terms []string
	for _, t := range strings.Fields(query) {
		terms = append(terms, `"`+strings.ReplaceAll(t, `"`, `""`)+`"*`)
	}
	if len(terms) == 0 {
		return nil, nil
	}
	rows, err := db.Query(`
SELECT d.id, d.root, d.relpath, d.title, d.hash, d.mtime, d.words, `+docStatusExpr+`,
       snippet(docs_fts, 1, '<mark>', '</mark>', ' … ', 18)
FROM docs_fts
JOIN docs d ON d.id = docs_fts.rowid
LEFT JOIN read_state r ON r.doc_id = d.id
WHERE docs_fts MATCH ?
ORDER BY rank`, strings.Join(terms, " "))
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

func getDoc(db *sql.DB, rootName, rel string) (*Doc, error) {
	row := db.QueryRow(`
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

func markRead(db *sql.DB, docID int64, hash string) error {
	_, err := db.Exec(`
INSERT INTO read_state(doc_id, read_at, hash_read) VALUES(?,?,?)
ON CONFLICT(doc_id) DO UPDATE SET read_at=excluded.read_at, hash_read=excluded.hash_read`,
		docID, time.Now().Unix(), hash)
	return err
}

func markUnread(db *sql.DB, docID int64) error {
	_, err := db.Exec(`DELETE FROM read_state WHERE doc_id=?`, docID)
	return err
}

// stripFrontmatter removes a leading YAML frontmatter block if present.
func stripFrontmatter(s string) string {
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
