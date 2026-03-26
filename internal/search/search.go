package search

import (
	"context"
	"fmt"
	"sort"

	"github.com/agent-mem/agent-mem/internal/database"
	"github.com/agent-mem/agent-mem/internal/gemini"
)

// Searcher performs hybrid search across observations and summaries.
type Searcher struct {
	db     *database.DB
	gemini *gemini.Client
}

// NewSearcher creates a new hybrid searcher.
func NewSearcher(db *database.DB, gemini *gemini.Client) *Searcher {
	return &Searcher{db: db, gemini: gemini}
}

// Search performs hybrid FTS + semantic search across observations and summaries.
func (s *Searcher) Search(ctx context.Context, query, project string, limit int) ([]database.SearchResult, error) {
	if s.gemini == nil {
		return nil, fmt.Errorf("gemini client not configured")
	}

	queryEmbedding, err := s.gemini.Embed(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("embed query: %w", err)
	}

	// Search observations and summaries in parallel
	obsCh := make(chan []database.SearchResult, 1)
	sumCh := make(chan []database.SearchResult, 1)
	errCh := make(chan error, 2)

	go func() {
		results, err := s.db.HybridSearch(ctx, query, queryEmbedding, project, limit)
		if err != nil {
			errCh <- err
			obsCh <- nil
			return
		}
		errCh <- nil
		obsCh <- results
	}()

	go func() {
		results, err := s.db.HybridSearchSummaries(ctx, query, queryEmbedding, project, limit)
		if err != nil {
			errCh <- err
			sumCh <- nil
			return
		}
		errCh <- nil
		sumCh <- results
	}()

	// Collect results
	obsResults := <-obsCh
	sumResults := <-sumCh
	for i := 0; i < 2; i++ {
		if err := <-errCh; err != nil {
			return nil, err
		}
	}

	// Merge and re-sort by combined score
	all := append(obsResults, sumResults...)
	sort.Slice(all, func(i, j int) bool {
		return all[i].CombinedScore > all[j].CombinedScore
	})

	if len(all) > limit {
		all = all[:limit]
	}
	return all, nil
}
