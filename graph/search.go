package graph

import (
	"database/sql"
	"sort"
	"strings"
	"unicode"
)

// SearchHit is a feature that matches a search, with what to show for it.
type SearchHit struct {
	Name    string
	Summary string
	Members int
	score   int
}

// searchStopwords are words too common to say which feature a search means.
var searchStopwords = map[string]bool{
	"the": true, "and": true, "for": true, "with": true, "from": true, "that": true,
	"this": true, "into": true, "what": true, "how": true, "does": true, "are": true,
	"anything": true, "about": true, "feature": true, "features": true, "map": true,
}

// Search finds the features a few words are about, best first: a word in a
// feature's name counts most, then its summary, then its notes and member
// names. It is how a session looks for a map it does not know the name of,
// so the startup list can stay short however many maps a project holds.
func Search(db *sql.DB, query string, limit int) ([]SearchHit, error) {
	words := searchWords(query)
	if len(words) == 0 {
		return nil, nil
	}

	rows, err := db.Query(`
		SELECT f.name, f.summary,
		       (SELECT count(*) FROM members m WHERE m.feature_id = f.id),
		       coalesce((SELECT group_concat(body, ' ') FROM notes n WHERE n.feature_id = f.id), ''),
		       coalesce((SELECT group_concat(ref || ' ' || path, ' ') FROM members m WHERE m.feature_id = f.id), '')
		FROM features f`)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()

	var hits []SearchHit
	for rows.Next() {
		var h SearchHit
		var notes, members string
		if err := rows.Scan(&h.Name, &h.Summary, &h.Members, &notes, &members); err != nil {
			return nil, err
		}
		name, summary := tokens(h.Name), tokens(h.Summary)
		noteWords, memberWords := tokens(notes), tokens(members)

		matched := 0
		for _, w := range words {
			s := 0
			switch {
			case hasWord(name, w):
				s = 5
			case hasWord(summary, w):
				s = 3
			case hasWord(memberWords, w):
				s = 2
			case hasWord(noteWords, w):
				s = 1
			}
			if s > 0 {
				matched++
				h.score += s
			}
		}
		// Every word must be found somewhere: "capsule sizing" is not about
		// every feature that mentions sizing.
		if matched == len(words) {
			hits = append(hits, h)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	sort.SliceStable(hits, func(i, j int) bool {
		if hits[i].score != hits[j].score {
			return hits[i].score > hits[j].score
		}
		return hits[i].Name < hits[j].Name
	})
	if limit > 0 && len(hits) > limit {
		hits = hits[:limit]
	}
	return hits, nil
}

// searchWords splits a query into lower-cased words worth matching: two
// letters or more, not a stopword. A trailing plural "s" is dropped so
// "capsules" finds "capsule".
func searchWords(query string) []string {
	var out []string
	seen := map[string]bool{}
	for _, w := range tokens(query) {
		if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			w = strings.TrimSuffix(w, "s")
		}
		if len(w) < 2 || searchStopwords[w] || seen[w] {
			continue
		}
		seen[w] = true
		out = append(out, w)
	}
	return out
}

// tokens splits text into lower-cased words, breaking on anything that is
// not a letter or digit, so a path or snake_case name yields its parts.
func tokens(text string) []string {
	return strings.FieldsFunc(strings.ToLower(text), func(r rune) bool {
		return !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
}

// hasWord reports whether any word begins with w — "ask" finds "ask" and
// "asked" but not "task". A two-letter w ("ai", "ui", "db") must be a whole
// word, since as a prefix it would match half the dictionary.
func hasWord(words []string, w string) bool {
	for _, t := range words {
		if t == w || (len(w) > 2 && strings.HasPrefix(t, w)) {
			return true
		}
	}
	return false
}
