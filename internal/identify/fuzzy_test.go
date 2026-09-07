package identify

import (
	"math"
	"testing"
)

// ids returns the TMDB ids of a ranked list, in order, for comparing rankings
// without spelling out every field.
func ids(ranked []RankedCandidate) []int {
	out := make([]int, len(ranked))
	for i, r := range ranked {
		out[i] = r.TMDBID
	}
	return out
}

// equalIDs compares two id slices element-wise.
func equalIDs(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestNormalize(t *testing.T) {
	tests := []struct {
		name      string
		label     string
		wantQuery string
		wantYear  int
	}{
		{
			name:      "studio prefix and blu ray noise",
			label:     "MARVELS_IRON_MAN_3_BLU_RAY",
			wantQuery: "iron man 3",
		},
		{
			name:      "single word bluray token",
			label:     "THE_MATRIX_BLURAY",
			wantQuery: "the matrix",
		},
		{
			name:      "year extracted and removed",
			label:     "BLADE_RUNNER_2049_BLU_RAY",
			wantQuery: "blade runner",
			wantYear:  2049,
		},
		{
			name:      "disc marker swallows its number",
			label:     "LORD_OF_THE_RINGS_DISC_1",
			wantQuery: "lord of the rings",
		},
		{
			name:      "dots dashes and mixed case",
			label:     "Spider-Man.Far.From.Home.1080p",
			wantQuery: "spider man far from home",
		},
		{
			name:      "edition and region boilerplate",
			label:     "ALIENS_SPECIAL_EDITION_R1_NTSC_DVD",
			wantQuery: "aliens special",
		},
		{
			name:      "colons and apostrophes in a title",
			label:     "MARVEL'S_THE_AVENGERS",
			wantQuery: "s the avengers",
		},
		{
			name:      "year only label keeps the year as the title",
			label:     "2012",
			wantQuery: "2012",
		},
		{
			name:      "already normalized query is unchanged",
			label:     "iron man 3",
			wantQuery: "iron man 3",
		},
		{
			name:      "pure noise leaves an empty query",
			label:     "BLU_RAY_DISC",
			wantQuery: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			query, year := Normalize(tt.label)
			if query != tt.wantQuery {
				t.Errorf("Normalize(%q) query = %q, want %q", tt.label, query, tt.wantQuery)
			}
			if year != tt.wantYear {
				t.Errorf("Normalize(%q) year = %d, want %d", tt.label, year, tt.wantYear)
			}
		})
	}
}

func TestNormalizeIsIdempotent(t *testing.T) {
	labels := []string{
		"MARVELS_IRON_MAN_3_BLU_RAY",
		"Spider-Man.Far.From.Home.1080p",
		"THE_MATRIX_BLURAY",
	}
	for _, label := range labels {
		once, _ := Normalize(label)
		twice, year := Normalize(once)
		if twice != once {
			t.Errorf("Normalize(Normalize(%q)) = %q, want %q", label, twice, once)
		}
		if year != 0 {
			t.Errorf("Normalize(%q) re-extracted year %d", once, year)
		}
	}
}

// candidates used across the ranking tests. ironMan3 is the correct answer for
// an Iron Man 3 disc; the others are the sort of near-misses TMDB returns.
var (
	ironMan3    = Candidate{TMDBID: 68721, Title: "Iron Man 3", Year: 2013, Popularity: 40}
	ironMan     = Candidate{TMDBID: 1726, Title: "Iron Man", Year: 2008, Popularity: 60}
	ironMan2    = Candidate{TMDBID: 10138, Title: "Iron Man 2", Year: 2010, Popularity: 50}
	ageOfUltron = Candidate{TMDBID: 99861, Title: "Avengers: Age of Ultron", Year: 2015, Popularity: 45}
)

func TestRank(t *testing.T) {
	tests := []struct {
		name       string
		label      string
		candidates []Candidate
		wantOrder  []int
		// wantTopMin/wantTopMax bound the winning score; zero means unchecked.
		wantTopMin float64
		wantTopMax float64
	}{
		{
			name:       "exact match scores near one",
			label:      "IRON_MAN_3",
			candidates: []Candidate{ironMan3},
			wantOrder:  []int{68721},
			wantTopMin: 0.99,
			wantTopMax: 1.0,
		},
		{
			name:       "noise and studio prefix stripped before matching",
			label:      "MARVELS_IRON_MAN_3_BLU_RAY_DISC_1",
			candidates: []Candidate{ironMan3},
			wantOrder:  []int{68721},
			wantTopMin: 0.99,
			wantTopMax: 1.0,
		},
		{
			name:       "no good match scores low",
			label:      "MARVELS_IRON_MAN_3_BLU_RAY",
			candidates: []Candidate{ageOfUltron},
			wantOrder:  []int{99861},
			wantTopMax: 0.3,
		},
		{
			// The right sequel wins outright. Behind it, "Iron Man" outranks
			// "Iron Man 2" because it merely omits the label's numeral while
			// "Iron Man 2" contradicts it, and an unrelated Marvel film comes
			// last.
			name:       "sequels ranked by numeral",
			label:      "MARVELS_IRON_MAN_3_BLU_RAY",
			candidates: []Candidate{ironMan, ironMan2, ageOfUltron, ironMan3},
			wantOrder:  []int{68721, 1726, 10138, 99861},
			wantTopMin: 0.99,
		},
		{
			name:  "correct film is not first from TMDB but wins after ranking",
			label: "THE_THING_1982_BLU_RAY",
			candidates: []Candidate{
				{TMDBID: 83533, Title: "The Thing", Year: 2011, Popularity: 70},
				{TMDBID: 1091, Title: "The Thing", Year: 1982, Popularity: 30},
			},
			wantOrder:  []int{1091, 83533},
			wantTopMin: 0.99,
		},
		{
			name:  "year in the label separates same-titled remakes",
			label: "TRUE_GRIT_2010",
			candidates: []Candidate{
				{TMDBID: 10951, Title: "True Grit", Year: 1969, Popularity: 20},
				{TMDBID: 44264, Title: "True Grit", Year: 2010, Popularity: 20},
			},
			wantOrder: []int{44264, 10951},
		},
		{
			name:  "subtitle in the TMDB title still matches a short label",
			label: "STAR_WARS_EPISODE_V_BLU_RAY",
			candidates: []Candidate{
				{TMDBID: 1891, Title: "Star Wars: Episode V - The Empire Strikes Back", Year: 1980, Popularity: 50},
				{TMDBID: 11, Title: "Star Wars", Year: 1977, Popularity: 80},
				{TMDBID: 601, Title: "E.T. the Extra-Terrestrial", Year: 1982, Popularity: 40},
			},
			wantOrder:  []int{1891, 11, 601},
			wantTopMin: 0.6,
		},
		{
			name:  "original title matches when the localized one does not",
			label: "CROUCHING_TIGER_HIDDEN_DRAGON_BLU_RAY",
			candidates: []Candidate{
				{TMDBID: 146, Title: "Wo hu cang long", OriginalTitle: "Crouching Tiger, Hidden Dragon", Year: 2000, Popularity: 30},
				{TMDBID: 9502, Title: "Kung Fu Panda", Year: 2008, Popularity: 60},
			},
			wantOrder:  []int{146, 9502},
			wantTopMin: 0.99,
		},
		{
			name:  "ties broken by popularity then id",
			label: "THE_MATRIX",
			candidates: []Candidate{
				{TMDBID: 300, Title: "The Matrix", Popularity: 10},
				{TMDBID: 100, Title: "The Matrix", Popularity: 10},
				{TMDBID: 200, Title: "The Matrix", Popularity: 90},
			},
			wantOrder: []int{200, 100, 300},
		},
		{
			name:       "empty candidate list ranks to an empty list",
			label:      "IRON_MAN_3",
			candidates: nil,
			wantOrder:  []int{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ranked := Rank(tt.label, tt.candidates)

			if len(ranked) != len(tt.candidates) {
				t.Fatalf("Rank returned %d candidates, want %d", len(ranked), len(tt.candidates))
			}
			if got := ids(ranked); !equalIDs(got, tt.wantOrder) {
				t.Errorf("Rank(%q) order = %v, want %v (scores %v)", tt.label, got, tt.wantOrder, scoresOf(ranked))
			}

			for i, r := range ranked {
				if r.Score < 0 || r.Score > 1 {
					t.Errorf("candidate %d score %v outside [0,1]", r.TMDBID, r.Score)
				}
				if i > 0 && ranked[i-1].Score < r.Score {
					t.Errorf("scores not descending at %d: %v", i, scoresOf(ranked))
				}
			}

			if len(ranked) == 0 {
				return
			}
			if tt.wantTopMin > 0 && ranked[0].Score < tt.wantTopMin {
				t.Errorf("top score %v < want min %v", ranked[0].Score, tt.wantTopMin)
			}
			if tt.wantTopMax > 0 && ranked[0].Score > tt.wantTopMax {
				t.Errorf("top score %v > want max %v", ranked[0].Score, tt.wantTopMax)
			}
		})
	}
}

// scoresOf extracts scores for failure messages.
func scoresOf(ranked []RankedCandidate) []float64 {
	out := make([]float64, len(ranked))
	for i, r := range ranked {
		out[i] = r.Score
	}
	return out
}

func TestRankIsDeterministic(t *testing.T) {
	candidates := []Candidate{ironMan, ironMan2, ageOfUltron, ironMan3}
	first := ids(Rank("MARVELS_IRON_MAN_3_BLU_RAY", candidates))
	for i := 0; i < 20; i++ {
		if got := ids(Rank("MARVELS_IRON_MAN_3_BLU_RAY", candidates)); !equalIDs(got, first) {
			t.Fatalf("run %d order = %v, want %v", i, got, first)
		}
	}
}

func TestRankDoesNotMutateInput(t *testing.T) {
	candidates := []Candidate{ironMan, ironMan2, ironMan3}
	before := append([]Candidate{}, candidates...)
	Rank("IRON_MAN_3", candidates)
	for i := range candidates {
		if candidates[i] != before[i] {
			t.Errorf("input candidate %d mutated: %+v, want %+v", i, candidates[i], before[i])
		}
	}
}

func TestExactMatchScoresOne(t *testing.T) {
	ranked := Rank("IRON_MAN_3_BLU_RAY", []Candidate{ironMan3})
	if math.Abs(ranked[0].Score-1) > 1e-9 {
		t.Errorf("exact match score = %v, want 1", ranked[0].Score)
	}
}

func TestTopN(t *testing.T) {
	ranked := Rank("MARVELS_IRON_MAN_3_BLU_RAY", []Candidate{ironMan, ironMan2, ageOfUltron, ironMan3})

	tests := []struct {
		name string
		n    int
		want int
	}{
		{name: "top three for the confirm view", n: 3, want: 3},
		{name: "n larger than the list", n: 10, want: 4},
		{name: "zero", n: 0, want: 0},
		{name: "negative clamps to zero", n: -1, want: 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := len(TopN(ranked, tt.n)); got != tt.want {
				t.Errorf("len(TopN(ranked, %d)) = %d, want %d", tt.n, got, tt.want)
			}
		})
	}

	if got := TopN(ranked, 3); got[0].TMDBID != 68721 {
		t.Errorf("TopN kept the wrong head: %v", ids(got))
	}
	if got := len(TopN(nil, 3)); got != 0 {
		t.Errorf("TopN(nil, 3) length = %d, want 0", got)
	}
}

func TestShouldAutoConfirm(t *testing.T) {
	threshold := func(v float64) *float64 { return &v }

	ranked := []RankedCandidate{
		{Candidate: ironMan3, Score: 0.9},
		{Candidate: ironMan2, Score: 0.4},
	}

	tests := []struct {
		name      string
		ranked    []RankedCandidate
		threshold *float64
		want      bool
	}{
		{
			name:      "nil threshold always requires manual confirmation",
			ranked:    ranked,
			threshold: nil,
			want:      false,
		},
		{
			name:      "nil threshold with a perfect score still requires confirmation",
			ranked:    []RankedCandidate{{Candidate: ironMan3, Score: 1}},
			threshold: nil,
			want:      false,
		},
		{
			name:      "threshold above the top score",
			ranked:    ranked,
			threshold: threshold(0.95),
			want:      false,
		},
		{
			name:      "threshold equal to the top score",
			ranked:    ranked,
			threshold: threshold(0.9),
			want:      true,
		},
		{
			name:      "threshold below the top score",
			ranked:    ranked,
			threshold: threshold(0.75),
			want:      true,
		},
		{
			name:      "empty candidate list with a threshold set",
			ranked:    nil,
			threshold: threshold(0.5),
			want:      false,
		},
		{
			name:      "empty candidate list with a zero threshold",
			ranked:    []RankedCandidate{},
			threshold: threshold(0),
			want:      false,
		},
		{
			name:      "only the top candidate is consulted",
			ranked:    []RankedCandidate{{Candidate: ironMan2, Score: 0.2}, {Candidate: ironMan3, Score: 1}},
			threshold: threshold(0.5),
			want:      false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := ShouldAutoConfirm(tt.ranked, tt.threshold); got != tt.want {
				t.Errorf("ShouldAutoConfirm = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestShouldAutoConfirmUsesRankOutput(t *testing.T) {
	ranked := Rank("MARVELS_IRON_MAN_3_BLU_RAY", []Candidate{ironMan, ironMan2, ageOfUltron, ironMan3})
	high := 0.9
	if !ShouldAutoConfirm(ranked, &high) {
		t.Errorf("a clean disc label should auto-confirm at 0.9, top score %v", ranked[0].Score)
	}

	weak := Rank("UNTITLED_DISC_1", []Candidate{ageOfUltron})
	if ShouldAutoConfirm(weak, &high) {
		t.Errorf("a junk disc label should not auto-confirm at 0.9, top score %v", weak[0].Score)
	}
}
