// SPDX-FileCopyrightText: Copyright The Miniflux Authors. All rights reserved.
// SPDX-License-Identifier: Apache-2.0

package ui // import "miniflux.app/v2/internal/ui"

import (
	"bytes"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"time"

	"github.com/yuin/goldmark"

	"miniflux.app/v2/internal/config"
	"miniflux.app/v2/internal/http/request"
	"miniflux.app/v2/internal/http/response"
	"miniflux.app/v2/internal/locale"
	"miniflux.app/v2/internal/mediaproxy"
	"miniflux.app/v2/internal/model"
	"miniflux.app/v2/internal/reader/readingtime"
	"miniflux.app/v2/internal/reader/sanitizer"
)

func (h *handler) fetchContentViaDefuddle(w http.ResponseWriter, r *http.Request) {
	loggedUserID := request.UserID(r)
	entryID := request.RouteInt64Param(r, "entryID")

	entryBuilder := h.store.NewEntryQueryBuilder(loggedUserID)
	entryBuilder.WithEntryID(entryID)
	entryBuilder.WithoutStatus(model.EntryStatusRemoved)

	entry, err := entryBuilder.GetEntry()
	if err != nil {
		response.JSONServerError(w, r, err)
		return
	}

	if entry == nil {
		response.JSONNotFound(w, r)
		return
	}

	user, err := h.store.UserByID(loggedUserID)
	if err != nil {
		response.JSONServerError(w, r, err)
		return
	}

	// Fetch markdown content from Defuddle API.
	defuddleURL := fmt.Sprintf("https://defuddle.md/%s", entry.URL)

	slog.Debug("Fetching content via Defuddle",
		slog.Int64("user_id", loggedUserID),
		slog.Int64("entry_id", entryID),
		slog.String("defuddle_url", defuddleURL),
	)

	httpClient := &http.Client{
		Timeout: time.Duration(config.Opts.HTTPClientTimeout()) * time.Second,
	}

	resp, err := httpClient.Get(defuddleURL)
	if err != nil {
		slog.Error("Defuddle request failed",
			slog.Int64("entry_id", entryID),
			slog.Any("error", err),
		)
		response.JSONServerError(w, r, fmt.Errorf("defuddle request failed: %w", err))
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		slog.Error("Defuddle returned non-200 status",
			slog.Int64("entry_id", entryID),
			slog.Int("status_code", resp.StatusCode),
		)
		response.JSONServerError(w, r, fmt.Errorf("defuddle returned status %d", resp.StatusCode))
		return
	}

	markdownBytes, err := io.ReadAll(resp.Body)
	if err != nil {
		response.JSONServerError(w, r, fmt.Errorf("failed to read defuddle response: %w", err))
		return
	}

	// Convert Markdown to HTML using goldmark.
	var htmlBuf bytes.Buffer
	md := goldmark.New()
	if err := md.Convert(markdownBytes, &htmlBuf); err != nil {
		response.JSONServerError(w, r, fmt.Errorf("failed to convert markdown to HTML: %w", err))
		return
	}

	htmlContent := htmlBuf.String()

	// Sanitize the HTML content.
	htmlContent = sanitizer.SanitizeHTML(entry.URL, htmlContent, &sanitizer.SanitizerOptions{
		OpenLinksInNewTab: user.OpenExternalLinksInNewTab,
	})

	// Update entry content and reading time.
	entry.Content = htmlContent
	if user.ShowReadingTime {
		entry.ReadingTime = readingtime.EstimateReadingTime(entry.Content, user.DefaultReadingSpeed, user.CJKReadingSpeed)
	}

	if err := h.store.UpdateEntryTitleAndContent(entry); err != nil {
		response.JSONServerError(w, r, err)
		return
	}

	readingTime := locale.NewPrinter(user.Language).Plural("entry.estimated_reading_time", entry.ReadingTime, entry.ReadingTime)

	response.JSON(w, r, map[string]string{"content": mediaproxy.RewriteDocumentWithRelativeProxyURL(entry.Content), "reading_time": readingTime})
}
