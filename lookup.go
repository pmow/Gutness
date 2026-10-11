package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

type Suggestion struct {
	Title, Author, Year string
}

// LookupOpenLibrary asks Open Library for a match. It is optional: any
// failure or timeout just returns an error and changes nothing.
func LookupOpenLibrary(ctx context.Context, title, author string) (*Suggestion, error) {
	if strings.TrimSpace(title) == "" {
		return nil, fmt.Errorf("need at least a title to look up")
	}
	ctx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()
	q := url.Values{"title": {title}, "limit": {"1"}, "fields": {"title,author_name,first_publish_year"}}
	if author != "" {
		q.Set("author", author)
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", "https://openlibrary.org/search.json?"+q.Encode(), nil)
	req.Header.Set("User-Agent", "aural-audiobook-organizer/0.1")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Open Library did not respond: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("Open Library returned %s", resp.Status)
	}
	var out struct {
		Docs []struct {
			Title  string   `json:"title"`
			Author []string `json:"author_name"`
			Year   int      `json:"first_publish_year"`
		} `json:"docs"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, fmt.Errorf("unexpected reply from Open Library: %w", err)
	}
	if len(out.Docs) == 0 {
		return nil, fmt.Errorf("no match found")
	}
	d := out.Docs[0]
	s := &Suggestion{Title: d.Title}
	if len(d.Author) > 0 {
		s.Author = d.Author[0]
	}
	if d.Year > 0 {
		s.Year = itoa(d.Year)
	}
	return s, nil
}
