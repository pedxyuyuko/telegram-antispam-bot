package telegram

import (
	"context"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"golang.org/x/net/html"
)

const maxPublicPageBytes = 512 << 10

// PublicPage is the audited outcome of reading a t.me preview page. Only the
// visible display fields are kept; the page HTML itself is not archived.
type PublicPage struct {
	URL         string `json:"url"`
	HTTPStatus  int    `json:"http_status,omitempty"`
	Found       bool   `json:"found"`
	Kind        string `json:"kind,omitempty"`
	Title       string `json:"title,omitempty"`
	Description string `json:"description,omitempty"`
	Audience    string `json:"audience,omitempty"`
	Error       string `json:"error,omitempty"`
	DurationMS  int64  `json:"duration_ms"`
}

var (
	invitePath         = regexp.MustCompile(`^/(\+[A-Za-z0-9_-]{1,64}|joinchat/[A-Za-z0-9_-]{1,64})$`)
	genericDescription = regexp.MustCompile(`^You can (contact|view and join) @[A-Za-z0-9_]+ right away\.$`)
)

// PublicPage reads the t.me preview of a username or invite link. It shares
// the redirect-refusing file transport and never follows Telegram's redirect
// for unknown names.
func (c *Client) PublicPage(ctx context.Context, ref Reference) (page PublicPage) {
	start := time.Now()
	defer func() { page.DurationMS = time.Since(start).Milliseconds() }()
	base := strings.TrimRight(c.PublicPageBase, "/")
	switch {
	case ref.Username != "" && referenceUsername.MatchString(ref.Username):
		page.URL = base + "/" + strings.ToLower(ref.Username)
	case strings.HasPrefix(ref.Key, "invite:"):
		u, err := url.Parse(ref.URL)
		if err != nil || !invitePath.MatchString(u.EscapedPath()) {
			page.Error = "unsupported invite link"
			return page
		}
		page.URL = base + u.EscapedPath()
	default:
		page.Error = "reference has no public page"
		return page
	}
	if c.files == nil {
		page.Error = "public page lookup requires redirect-controlled HTTP transport"
		return page
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, page.URL, nil)
	if err != nil {
		page.Error = "invalid public page request"
		return page
	}
	// English labels keep member and subscriber counts recognizable.
	req.Header.Set("Accept-Language", "en")
	resp, err := c.files.Do(req)
	if err != nil {
		page.Error = c.safe(err).Error()
		return page
	}
	defer resp.Body.Close()
	page.HTTPStatus = resp.StatusCode
	if resp.StatusCode != http.StatusOK {
		page.Error = fmt.Sprintf("public page HTTP status %d", resp.StatusCode)
		return page
	}
	if media, _, err := mime.ParseMediaType(resp.Header.Get("Content-Type")); err != nil || media != "text/html" {
		page.Error = "public page is not HTML"
		return page
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxPublicPageBytes+1))
	if err != nil {
		page.Error = c.safe(err).Error()
		return page
	}
	if len(raw) > maxPublicPageBytes {
		page.Error = "public page exceeds size budget"
		return page
	}
	parsed, err := parsePublicPage(strings.NewReader(string(raw)))
	if err != nil {
		page.Error = err.Error()
		return page
	}
	parsed.URL, parsed.HTTPStatus = page.URL, page.HTTPStatus
	if !parsed.Found {
		parsed.Error = "public page has no profile"
	}
	return parsed
}

func hasClass(n *html.Node, class string) bool {
	for _, a := range n.Attr {
		if a.Key == "class" {
			for _, c := range strings.Fields(a.Val) {
				if c == class {
					return true
				}
			}
		}
	}
	return false
}

func attr(n *html.Node, key string) string {
	for _, a := range n.Attr {
		if a.Key == key {
			return a.Val
		}
	}
	return ""
}

// visibleText joins text nodes, turning <br> into line breaks. Icon elements
// such as the verified mark are decoration, not part of the name.
func visibleText(n *html.Node) string {
	var b strings.Builder
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		switch {
		case n.Type == html.TextNode:
			b.WriteString(n.Data)
		case n.Type == html.ElementNode && n.Data == "br":
			b.WriteString("\n")
		case n.Type == html.ElementNode && n.Data == "i":
			return
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(n)
	lines := strings.Split(b.String(), "\n")
	for i := range lines {
		lines[i] = strings.TrimSpace(lines[i])
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}

func clip(s string, limit int) string {
	if r := []rune(s); len(r) > limit {
		return string(r[:limit])
	}
	return s
}

// parsePublicPage reads the profile card of a t.me preview page. A page
// without a title card is Telegram's placeholder for an unknown name.
func parsePublicPage(r io.Reader) (PublicPage, error) {
	doc, err := html.Parse(r)
	if err != nil {
		return PublicPage{}, fmt.Errorf("parse public page: %w", err)
	}
	var page PublicPage
	var ogDescription, button string
	var walk func(*html.Node)
	walk = func(n *html.Node) {
		if n.Type == html.ElementNode {
			switch {
			case n.Data == "meta" && attr(n, "property") == "og:description":
				ogDescription = attr(n, "content")
			case n.Data == "div" && hasClass(n, "tgme_page_title"):
				page.Title = visibleText(n)
				page.Found = page.Title != ""
			case n.Data == "div" && hasClass(n, "tgme_page_description"):
				page.Description = visibleText(n)
			case n.Data == "div" && hasClass(n, "tgme_page_extra"):
				page.Audience = visibleText(n)
			case n.Data == "a" && hasClass(n, "tgme_action_button_new"):
				button = visibleText(n)
			}
		}
		for child := n.FirstChild; child != nil; child = child.NextSibling {
			walk(child)
		}
	}
	walk(doc)
	if !page.Found {
		return PublicPage{}, nil
	}
	if page.Description == "" {
		page.Description = strings.TrimSpace(strings.ReplaceAll(ogDescription, "\t", "\n"))
	}
	if genericDescription.MatchString(page.Description) {
		page.Description = ""
	}
	switch {
	case button == "Start Bot":
		page.Kind = "bot"
	case button == "Send Message":
		page.Kind = "user"
	case strings.Contains(page.Audience, "subscriber") || button == "Join Channel":
		page.Kind = "channel"
	case strings.Contains(page.Audience, "member") || button == "Join Group":
		page.Kind = "group"
	}
	if page.Kind != "group" && page.Kind != "channel" {
		page.Audience = ""
	}
	page.Title, page.Description, page.Audience = clip(page.Title, 256), clip(page.Description, 2048), clip(page.Audience, 128)
	return page, nil
}

// MergePublicPage completes what the Bot API cannot read: other bots' and
// unseen users' descriptions and invite-link chats. A Bot API identity whose
// display name differs from the page keeps its own material.
func (l *ReferenceLookup) MergePublicPage(p PublicPage) {
	defer func() { l.Page = &p }()
	if !p.Found {
		return
	}
	if l.Status == "available" {
		if p.Title != l.Profile.Name {
			p.Error = "public page name does not match Bot API profile"
			return
		}
		if l.Profile.Bio == "" {
			l.Profile.Bio = p.Description
		}
		if l.Profile.Kind == "private" && (p.Kind == "bot" || p.Kind == "user") {
			l.Profile.Kind = p.Kind
		}
		if l.Profile.Audience == "" {
			l.Profile.Audience = p.Audience
		}
		return
	}
	l.Profile = ReferenceProfile{Username: l.Reference.Username, Name: p.Title, Bio: p.Description, Kind: p.Kind, Audience: p.Audience}
	l.Status = "available"
}
