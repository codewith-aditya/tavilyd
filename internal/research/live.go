package research

import (
	"context"
	"strings"

	"github.com/astralabs/tavilyd/internal/extract"
	"github.com/astralabs/tavilyd/internal/rerank"
	"github.com/astralabs/tavilyd/internal/search"
)

// LiveTools wires the research engine to the production search/extract/rerank
// pipeline, so proxying, SSRF guards and cleaning apply automatically.
type LiveTools struct {
	Searcher  *search.Client
	Extractor *extract.Extractor
	Scorer    *rerank.Scorer
	Chunker   *rerank.Chunker
}

// Search runs one discovery query and returns deduped candidates.
func (t *LiveTools) Search(ctx context.Context, query string, max int) ([]Source, error) {
	res, err := t.Searcher.Search(ctx, search.Query{Query: query, Categories: "general", Language: "en"})
	if err != nil {
		return nil, err
	}
	res = search.Filter(search.Dedupe(res), nil, nil)
	if len(res) > max {
		res = res[:max]
	}
	out := make([]Source, 0, len(res))
	for _, r := range res {
		out = append(out, Source{Title: r.Title, URL: r.URL, Snippet: r.Content, Score: r.Score})
	}
	return out, nil
}

// Read fetches a page through the guarded fetcher and returns its best chunks.
func (t *LiveTools) Read(ctx context.Context, rawURL, query string) (string, error) {
	art, _, err := t.Extractor.FetchPage(ctx, rawURL, "advanced", false)
	if err != nil {
		return "", err
	}
	md := strings.TrimSpace(art.Markdown)
	if md == "" {
		return "", nil
	}
	chunks := t.Chunker.Split(md)
	if len(chunks) > 0 && t.Scorer != nil {
		texts := make([]string, len(chunks))
		for i, ch := range chunks {
			texts[i] = ch.Text
		}
		scores, serr := t.Scorer.Rerank(ctx, query, texts)
		if serr == nil && len(scores) == len(chunks) {
			type scored struct {
				idx   int
				score float64
			}
			ranked := make([]scored, len(chunks))
			for i := range chunks {
				ranked[i] = scored{idx: i, score: scores[i]}
			}
			// simple selection of the best three without importing sort logic twice
			for i := 0; i < len(ranked); i++ {
				for j := i + 1; j < len(ranked); j++ {
					if ranked[j].score > ranked[i].score {
						ranked[i], ranked[j] = ranked[j], ranked[i]
					}
				}
			}
			n := 3
			if len(ranked) < n {
				n = len(ranked)
			}
			pick := make([]int, 0, n)
			for i := 0; i < n; i++ {
				pick = append(pick, ranked[i].idx)
			}
			for i := 0; i < len(pick); i++ {
				for j := i + 1; j < len(pick); j++ {
					if pick[j] < pick[i] {
						pick[i], pick[j] = pick[j], pick[i]
					}
				}
			}
			parts := make([]string, 0, n)
			for _, idx := range pick {
				parts = append(parts, chunks[idx].Text)
			}
			md = strings.Join(parts, "\n\n")
		}
	}
	if len(md) > 6000 {
		md = md[:6000]
	}
	return md, nil
}
