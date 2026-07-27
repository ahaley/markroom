// Package peer implements markroom's server-to-server protocol: one machine
// advertises the markdown it holds, another mirrors it into a local cache so
// the documents stay readable after the origin goes offline.
//
// This file is the wire contract, shared by the handler in internal/web and
// the client below it so the two cannot drift.
package peer

// Protocol is the wire version. Additive fields never change it; optional
// transports are negotiated through Manifest.Caps instead.
const Protocol = 1

// MaxHops bounds how far a root may travel down a chain of servers, so a
// misconfigured mesh cannot grow paths without limit.
const MaxHops = 8

// Endpoint paths, so client and server can't disagree about them.
const (
	ManifestPath = "/api/peer/manifest"
	RawPath      = "/api/peer/raw/"
)

// Manifest is everything a server is willing to tell a peer about what it holds.
type Manifest struct {
	Protocol    int            `json:"protocol"`
	Server      ServerRef      `json:"server"`
	GeneratedAt int64          `json:"generated_at"`
	Caps        []string       `json:"caps,omitempty"`
	Roots       []RootManifest `json:"roots"`
}

// ServerRef identifies a markroom server. ID is stable and random; Name is
// user-chosen, for display, and may collide between machines.
type ServerRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// OriginRef points at where a root's documents were *originally* written. It
// is copied verbatim through every hop, so a doc three servers away is still
// attributed to the machine whose disk it lives on.
type OriginRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Root string `json:"root"` // the origin's own name for the root
}

// RootManifest is one root as advertised to a peer.
type RootManifest struct {
	// Name is the *advertising* server's local name for the root, and is the
	// handle used to fetch from it. For a re-exported mirror that means the
	// qualified form, e.g. "laptop:notes".
	Name   string    `json:"name"`
	Origin OriginRef `json:"origin"`
	// Hops lists the server IDs this root has passed through, origin first,
	// ending with the advertising server. Loop detection compares it.
	Hops   []string     `json:"hops"`
	Docs   []DocEntry   `json:"docs"`
	Assets []AssetEntry `json:"assets,omitempty"`
}

// DocEntry is one markdown file. Hash is sha256 of the raw file bytes — the
// same value the index already stores, so advertising it costs nothing and
// makes an incremental sync an exact comparison rather than a guess.
type DocEntry struct {
	Path  string `json:"path"`
	Hash  string `json:"hash"`
	MTime int64  `json:"mtime"`
	Size  int64  `json:"size"`
	Title string `json:"title,omitempty"`
	Words int    `json:"words,omitempty"`
}

// AssetEntry is one non-markdown file a document may reference. Assets are
// not hashed — hashing every image on every manifest build is not free, so
// they reuse the (mtime, size) change heuristic the scanner already trusts.
type AssetEntry struct {
	Path  string `json:"path"`
	MTime int64  `json:"mtime"`
	Size  int64  `json:"size"`
}

// HopsContain reports whether any of ids appears in hops.
func HopsContain(hops []string, ids map[string]bool) bool {
	for _, h := range hops {
		if ids[h] {
			return true
		}
	}
	return false
}
