package main

import (
	"context"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// rssFeed is deliberately small but accepts both RSS 2.0 and Atom feeds.
type rssFeed struct {
	Channel struct {
		Items []rssItem `xml:"item"`
	} `xml:"channel"`
	Entries []rssItem `xml:"entry"`
}
type rssItem struct {
	Title       string `xml:"title"`
	Link        string `xml:"link"`
	GUID        string `xml:"guid"`
	Description string `xml:"description"`
	Content     string `xml:"encoded"`
}

func importRSSFeed(ctx context.Context, rag *ragSystem, s appSettings, feedURL string, dryRun bool) (webImportResult, error) {
	var res webImportResult
	res.DryRun = dryRun
	u, err := isSafeWebURL(feedURL)
	if err != nil {
		return res, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return res, err
	}
	req.Header.Set("User-Agent", connectorUserAgent)
	resp, err := connectorHTTPClient.Do(req)
	if err != nil {
		return res, fmt.Errorf("rss fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return res, fmt.Errorf("rss fetch: HTTP %d", resp.StatusCode)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, int64(max(1, s.Import.MaxFileMB))*1024*1024))
	if err != nil {
		return res, err
	}
	var feed rssFeed
	if err := xml.Unmarshal(data, &feed); err != nil {
		return res, fmt.Errorf("rss parse: %w", err)
	}
	items := append(feed.Channel.Items, feed.Entries...)
	if len(items) == 0 {
		return res, fmt.Errorf("rss: no entries found")
	}
	pacer := newImportPacer(s.Import, 0)
	for _, item := range items {
		if err := ctx.Err(); err != nil {
			return res, err
		}
		if pacer.capReached() {
			res.Errors = append(res.Errors, pacer.capNote())
			break
		}
		if err := pacer.wait(ctx); err != nil {
			return res, err
		}
		id := strings.TrimSpace(item.GUID)
		if id == "" {
			id = strings.TrimSpace(item.Link)
		}
		if id == "" {
			id = item.Title
		}
		text := htmlToText(item.Content)
		if strings.TrimSpace(text) == "" {
			text = htmlToText(item.Description)
		}
		if strings.TrimSpace(text) == "" {
			continue
		}
		res.Pages++
		out, err := ingestDocument(rag, s, s.activeEmbedModel(), "rss:"+u.String()+":"+id, "rss_entry", strings.TrimSpace(item.Title), text, 0, dryRun)
		if err != nil {
			res.Errors = append(res.Errors, fmt.Sprintf("%s: %v", item.Title, err))
		} else if out.Skipped {
			res.Skipped++
		} else {
			res.Chunks += out.Chunks
		}
		pacer.count()
	}
	return res, nil
}
