// fuzzy.go turns an ugly disc-derived label ("MARVELS_IRON_MAN_3_BLU_RAY")
// into a search query plus an optional year, and scores the candidates TMDB
// returned against that label so the confirmation UI can show the best matches
// first.
//
// Everything here is pure and dependency-free: no HTTP, no manifest types, no
// third-party similarity library. Scores are normalized to the 0.0-1.0 range
// the manifest uses for identification.confidence and candidates[].score.
package identify

import (
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// noiseTokens are label fragments that describe the disc rather than the movie.
// They are dropped wherever they appear in a label. The list is deliberately
// conservative: a token only belongs here if it is vanishingly unlikely to be a
// meaningful word in a film title.
var noiseTokens = map[string]bool{
	// Media and format.
	"bluray": true, "blu": true, "ray": true, "bd": true, "bdrom": true,
	"bdmv": true, "bd25": true, "bd50": true, "dvd": true, "uhd": true,
	"hddvd": true, "4k": true, "2160p": true, "1080p": true, "1080i": true,
	"720p": true, "480p": true, "ntsc": true, "pal": true,
	// Disc/volume markers. "disc"/"disk" additionally swallow a following
	// number ("DISC 1"); see stripNoise.
	"disc": true, "disk": true, "side": true, "volume": true, "vol": true,
	// Region markers.
	"region": true, "r1": true, "r2": true, "r3": true, "r4": true,
	"r5": true, "r6": true,
	// Edition and marketing boilerplate.
	"remastered": true, "edition": true, "editions": true, "collectors": true,
	"collector": true, "anniversary": true, "deluxe": true, "unrated": true,
	"widescreen": true, "fullscreen": true, "letterbox": true,
}

// studioTokens are distributor or franchise-owner names that discs prepend to
// the title ("MARVELS_IRON_MAN_3"). They are stripped only from the front of a
// label, and never when doing so would leave nothing behind, so a title that
// genuinely starts with one of these words survives elsewhere in the string.
var studioTokens = map[string]bool{
	"marvel": true, "marvels": true, "disney": true, "disneys": true,
	"pixar": true, "dreamworks": true, "universal": true, "paramount": true,
	"warner": true, "wb": true, "lionsgate": true, "mgm": true,
	"columbia": true, "miramax": true, "touchstone": true, "dimension": true,
	"pictures": true, "studios": true, "presents": true,
}

// minYear and maxYear bound what a 4-digit token may be read as a release year.
const (
	minYear = 1900
	maxYear = 2099
)

// Normalize cleans a raw disc label into a TMDB search query and, when the
// label carries one, the release year.
//
// Separators (underscores, dots, dashes, punctuation) become spaces, the result
// is case-folded and whitespace-collapsed, disc-label noise tokens are dropped,
// and a leading studio name is removed. A 4-digit token in [1900, 2099] is
// returned as the year and removed from the query — but only when other words
// remain, so a label that is nothing but a year ("2012") stays a title.
//
// The returned year is 0 when the label has none. Normalize is idempotent:
// normalizing an already-normalized query yields the same query.
func Normalize(label string) (query string, year int) {
	tokens := tokenize(label)
	tokens = stripNoise(tokens)
	tokens = stripStudioPrefix(tokens)
	tokens, year = extractYear(tokens)
	return strings.Join(tokens, " "), year
}

// tokenize case-folds label and splits it on everything that is not a letter or
// digit, which covers underscores, dots, dashes, colons and apostrophes alike.
func tokenize(label string) []string {
	mapped := strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) {
			return unicode.ToLower(r)
		}
		return ' '
	}, label)
	return strings.Fields(mapped)
}

// stripNoise removes noiseTokens. A disc marker also consumes the number that
// follows it ("DISC 1"), which a bare token list would otherwise leave behind
// as a stray digit.
func stripNoise(tokens []string) []string {
	out := make([]string, 0, len(tokens))
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if !noiseTokens[tok] {
			out = append(out, tok)
			continue
		}
		if (tok == "disc" || tok == "disk" || tok == "side" || tok == "volume" || tok == "vol") &&
			i+1 < len(tokens) && isSmallNumber(tokens[i+1]) {
			i++
		}
	}
	return out
}

// stripStudioPrefix drops leading studio names while at least one other token
// would survive.
func stripStudioPrefix(tokens []string) []string {
	for len(tokens) > 1 && studioTokens[tokens[0]] {
		tokens = tokens[1:]
	}
	return tokens
}

// extractYear pulls the last year-like token out of tokens, leaving it in place
// when it is the only token left.
func extractYear(tokens []string) ([]string, int) {
	for i := len(tokens) - 1; i >= 0; i-- {
		y, ok := asYear(tokens[i])
		if !ok {
			continue
		}
		if len(tokens) == 1 {
			return tokens, 0
		}
		return append(append([]string{}, tokens[:i]...), tokens[i+1:]...), y
	}
	return tokens, 0
}

// asYear reports whether tok is a 4-digit number inside the plausible release
// year range.
func asYear(tok string) (int, bool) {
	if len(tok) != 4 {
		return 0, false
	}
	y, err := strconv.Atoi(tok)
	if err != nil || y < minYear || y > maxYear {
		return 0, false
	}
	return y, true
}

// isSmallNumber reports whether tok is a one- or two-digit number, the shape a
// disc index takes.
func isSmallNumber(tok string) bool {
	if len(tok) == 0 || len(tok) > 2 {
		return false
	}
	for _, r := range tok {
		if !unicode.IsDigit(r) {
			return false
		}
	}
	return true
}

// RankedCandidate is a TMDB candidate paired with how well it matched the disc
// label. Score is in [0, 1] and lands in the manifest as
// identification.candidates[].score, with the winner's score also written as
// identification.confidence.
type RankedCandidate struct {
	Candidate
	// Score is the match confidence in [0, 1]; 1 is an exact title (and year)
	// match against the normalized disc label.
	Score float64
}

// Scoring weights. Token overlap carries most of the decision because disc
// labels reorder and abbreviate words freely; the character-level ratio breaks
// ties between candidates that share the same tokens and catches near-misses
// like plurals or missing articles.
const (
	tokenWeight = 0.7
	charWeight  = 0.3
	// recallWeight splits the token score between recall (how many of the
	// label's words the title covers) and precision (how many of the title's
	// words the label accounts for). Recall leads because disc labels are
	// usually a truncation of the real title: "STAR_WARS_EPISODE_V" should
	// prefer "Star Wars: Episode V - The Empire Strikes Back", whose extra
	// subtitle costs precision, over the shorter "Star Wars", which covers
	// only half the label.
	recallWeight = 0.65
	// yearFloor is how much of the score a matching year contributes: a
	// matched year rescales the similarity into [yearFloor, 1].
	yearFloor = 0.1
	// yearMismatchFactor scales down a candidate whose year contradicts the
	// year in the label.
	yearMismatchFactor = 0.8
)

// Rank scores every candidate against discLabel and returns them sorted by
// descending score.
//
// discLabel is the raw label; Rank normalizes it (and each candidate title)
// itself, so callers can pass exactly what the disc or BDMV metadata reported.
// Ordering is deterministic: ties fall back to TMDB popularity, then to the
// lower TMDB id, so the same inputs always produce the same list.
//
// The input slice is not modified. Use TopN to take the handful the
// confirmation view shows; the full list belongs in the manifest.
func Rank(discLabel string, candidates []Candidate) []RankedCandidate {
	query, year := Normalize(discLabel)

	ranked := make([]RankedCandidate, 0, len(candidates))
	for _, c := range candidates {
		ranked = append(ranked, RankedCandidate{Candidate: c, Score: score(query, year, c)})
	}

	sort.SliceStable(ranked, func(i, j int) bool {
		a, b := ranked[i], ranked[j]
		if a.Score != b.Score {
			return a.Score > b.Score
		}
		if a.Popularity != b.Popularity {
			return a.Popularity > b.Popularity
		}
		return a.TMDBID < b.TMDBID
	})
	return ranked
}

// TopN returns at most n candidates from an already-ranked list — the top 3 for
// the confirmation view. It aliases ranked rather than copying it.
func TopN(ranked []RankedCandidate, n int) []RankedCandidate {
	if n < 0 {
		n = 0
	}
	if len(ranked) < n {
		n = len(ranked)
	}
	return ranked[:n]
}

// ShouldAutoConfirm reports whether the best-ranked candidate may be confirmed
// without a human.
//
// threshold is the configured tmdb.auto_confirm_threshold. A nil threshold
// means the operator never opted in, so confirmation is always manual; so does
// an empty candidate list or a top score below the floor.
func ShouldAutoConfirm(ranked []RankedCandidate, threshold *float64) bool {
	if threshold == nil || len(ranked) == 0 {
		return false
	}
	return ranked[0].Score >= *threshold
}

// score rates one candidate against a normalized query and the year read off
// the label. The candidate's localized and original titles are both tried and
// the better of the two wins, since disc labels frequently carry the original
// title.
func score(query string, year int, c Candidate) float64 {
	best := similarity(query, normalizeTitle(c.Title))
	if c.OriginalTitle != "" && !strings.EqualFold(c.OriginalTitle, c.Title) {
		if s := similarity(query, normalizeTitle(c.OriginalTitle)); s > best {
			best = s
		}
	}
	return applyYear(best, year, c.Year)
}

// normalizeTitle runs a TMDB title through the same cleanup as a disc label so
// the two are compared on equal terms: "Marvel's The Avengers" and
// "MARVELS_THE_AVENGERS" reduce to the same tokens.
func normalizeTitle(title string) string {
	query, _ := Normalize(title)
	return query
}

// applyYear folds the release year into a similarity score. A year present on
// both sides and matching rescales the score into [yearFloor, 1] — enough to
// separate the right sequel from its siblings without letting the year alone
// carry a bad title match. A contradicting year is penalized. When either side
// has no year the similarity stands unchanged.
func applyYear(base float64, labelYear, candidateYear int) float64 {
	if labelYear == 0 || candidateYear == 0 {
		return base
	}
	if labelYear == candidateYear {
		return yearFloor + (1-yearFloor)*base
	}
	return base * yearMismatchFactor
}

// similarity scores a normalized query against a normalized title in [0, 1],
// blending token-set similarity with a Levenshtein ratio over the whole string.
// The character-level term is what separates candidates that share the same
// tokens and catches near-misses like plurals or a missing article.
func similarity(query, title string) float64 {
	if query == "" || title == "" {
		return 0
	}
	if query == title {
		return 1
	}
	return tokenWeight*tokenSimilarity(query, title) + charWeight*levenshteinRatio(query, title)
}

// tokenSimilarity scores the overlap of two token sets, where query is the
// normalized label and title the normalized candidate title. Word order is
// ignored: labels reorder and drop words freely, so only which words appear on
// each side matters.
func tokenSimilarity(query, title string) float64 {
	setQ, setT := tokenSet(query), tokenSet(title)
	if len(setQ) == 0 || len(setT) == 0 {
		return 0
	}

	shared := 0
	for tok := range setQ {
		if setT[tok] {
			shared++
		}
	}
	if shared == 0 {
		return 0
	}

	recall := float64(shared) / float64(len(setQ))
	precision := float64(shared) / float64(len(setT))
	return recallWeight*recall + (1-recallWeight)*precision
}

// tokenSet splits an already-normalized string into a set of words.
func tokenSet(s string) map[string]bool {
	fields := strings.Fields(s)
	set := make(map[string]bool, len(fields))
	for _, f := range fields {
		set[f] = true
	}
	return set
}

// levenshteinRatio is 1 - distance/longest, giving 1 for identical strings and
// approaching 0 as they diverge.
func levenshteinRatio(a, b string) float64 {
	ra, rb := []rune(a), []rune(b)
	longest := len(ra)
	if len(rb) > longest {
		longest = len(rb)
	}
	if longest == 0 {
		return 1
	}
	return 1 - float64(levenshtein(ra, rb))/float64(longest)
}

// levenshtein is the standard edit distance, computed with two rows instead of
// the full matrix. Titles are short, so this is cheap enough to run per
// candidate.
func levenshtein(a, b []rune) int {
	if len(a) == 0 {
		return len(b)
	}
	if len(b) == 0 {
		return len(a)
	}

	prev := make([]int, len(b)+1)
	curr := make([]int, len(b)+1)
	for j := range prev {
		prev[j] = j
	}

	for i := 1; i <= len(a); i++ {
		curr[0] = i
		for j := 1; j <= len(b); j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			curr[j] = min(prev[j]+1, curr[j-1]+1, prev[j-1]+cost)
		}
		prev, curr = curr, prev
	}
	return prev[len(b)]
}
